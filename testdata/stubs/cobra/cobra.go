// Package cobra is a minimal offline stand-in for github.com/spf13/cobra: the
// command type and the calls the CLI rules look at. Execute runs the commands
// as the real library does, which keeps their functions reachable.
package cobra

type Command struct {
	Use   string
	Short string
	Run   func(cmd *Command, args []string)
	RunE  func(cmd *Command, args []string) error

	commands []*Command
}

func (c *Command) AddCommand(cmds ...*Command) { c.commands = append(c.commands, cmds...) }

func (c *Command) Execute() error {
	for _, cmd := range append([]*Command{c}, c.commands...) {
		switch {
		case cmd.RunE != nil:
			if err := cmd.RunE(cmd, nil); err != nil {
				return err
			}
		case cmd.Run != nil:
			cmd.Run(cmd, nil)
		}
	}
	return nil
}
