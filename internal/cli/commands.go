package cli

import "github.com/spf13/cobra"

func allCommands(root *cobra.Command) []*cobra.Command {
	commands := []*cobra.Command{root}
	for _, child := range root.Commands() {
		if child.Hidden || child.Name() == "help" || child.Name() == "completion" {
			continue
		}
		commands = append(commands, allCommands(child)...)
	}
	return commands
}
