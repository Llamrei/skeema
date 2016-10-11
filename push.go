package main

import (
	"fmt"

	"github.com/skeema/mycli"
	"github.com/skeema/tengo"
)

func init() {
	summary := "Alter tables on DBs to reflect the filesystem representation"
	desc := `Modifies the schemas on database instance(s) to match the corresponding
filesystem representation of them. This essentially performs the same diff logic
as ` + "`" + `skeema diff` + "`" + `, but then actually runs the generated DDL. You should generally
run ` + "`" + `skeema diff` + "`" + ` first to see what changes will be applied.

You may optionally pass an environment name as a CLI option. This will affect
which section of .skeema config files is used for processing. For example,
running ` + "`" + `skeema push production` + "`" + ` will apply config directives from the
[production] section of config files, as well as any sectionless directives
at the top of the file. If no environment name is supplied, only the sectionless
directives alone will be applied.`

	cmd := mycli.NewCommand("push", summary, desc, PushCommand, 0, 1, "environment")
	cmd.AddOption(mycli.BoolOption("verify", 0, true, "Test all generated ALTER statements on temporary schema to verify correctness"))
	CommandSuite.AddSubCommand(cmd)
}

func PushCommand(cfg *mycli.Config) error {
	environment := "production"
	if len(cfg.CLI.Args) > 0 {
		environment = cfg.CLI.Args[0]
	}
	AddGlobalConfigFiles(cfg, environment)

	dir, err := NewDir(".", cfg, environment)
	if err != nil {
		return err
	}

	mods := tengo.StatementModifiers{
		NextAutoInc: tengo.NextAutoIncIfIncreased,
	}

	// TODO: once sharding / service discovery lookup is supported, this should
	// use multiple worker goroutines all pulling instances off the channel
	for t := range dir.Targets(true, true) {
		if t.Err != nil {
			fmt.Printf("Skipping %s: %s\n", t.Dir, t.Err)
			t.Done()
			continue
		}
		fmt.Printf("\nPushing changes from %s/*.sql to %s %s...\n", t.Dir, t.Instance, t.SchemaFromDir.Name)
		diff, err := tengo.NewSchemaDiff(t.SchemaFromInstance, t.SchemaFromDir)
		if err != nil {
			t.Done()
			return err
		}
		if t.SchemaFromInstance == nil {
			// TODO: support CREATE DATABASE schema-level options
			var err error
			t.SchemaFromInstance, err = t.Instance.CreateSchema(t.SchemaFromDir.Name)
			if err != nil {
				t.Done()
				return fmt.Errorf("Error creating schema %s on %s: %s", t.SchemaFromDir.Name, t.Instance, err)
			}
			fmt.Printf("%s;\n", t.SchemaFromDir.CreateStatement())
		} else if len(diff.TableDiffs) == 0 {
			fmt.Println("(nothing to do)")
			t.Done()
			continue
		}

		if t.Dir.Config.GetBool("verify") && len(diff.TableDiffs) > 0 {
			if err := t.verifyDiff(diff); err != nil {
				t.Done()
				return err
			}
		}

		db, err := t.Instance.Connect(t.SchemaFromDir.Name, "")
		if err != nil {
			t.Done()
			return err
		}
		var statementCounter int
		for _, td := range diff.TableDiffs {
			stmt := td.Statement(mods)
			if stmt != "" {
				statementCounter++
				_, err := db.Exec(stmt)
				if err != nil {
					t.Done()
					return fmt.Errorf("Error running statement \"%s\" on %s: %s", stmt, t.Instance, err)
				} else {
					fmt.Printf("%s;\n", stmt)
				}
			}
		}

		// If we had diffs but they were all no-ops due to StatementModifiers,
		// still display message about no actions taken
		if statementCounter == 0 {
			fmt.Println("(nothing to do)")
		}

		t.Done()
	}

	return nil
}
