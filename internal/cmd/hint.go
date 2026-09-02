package cmd

import (
	"fmt"

	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

var hintCmd = &cobra.Command{
	Use:     "hint",
	GroupID: GroupWorkspace,
	Short:   "Friendly reminder of the first-run commands",
	Long: `Print a short getting-started reminder: create a Town, pick Claude or
OpenCode, mix the Mayor with a different agent, start services, and add a Rig.

This is documentation you can re-read at the terminal. It does not change
any configuration.`,
	RunE: runHint,
}

func init() {
	rootCmd.AddCommand(hintCmd)
}

func runHint(_ *cobra.Command, _ []string) error {
	fmt.Print(hintText())
	return nil
}

func hintText() string {
	title := style.Bold.Render("Getting started")
	return title + `

Create a Town, pick an agent, start it, then add a project.

1. Create a Town

   gt install ~/my-town
   cd ~/my-town

2. Pick an agent

   Claude (the default):

   gt config default-agent claude

   OpenCode with gpt-oss:

   gt config agent set opencode "opencode -m ollama-cloud/gpt-oss:120b" --provider opencode
   gt config default-agent opencode

   Swap the OpenCode model string for whatever your provider offers.
   Run gt config default-agent list to see the other built-in agents.

   One agent for the Mayor, another for everyone else:

   gt config mix default=opencode mayor=claude

   That keeps OpenCode (gpt-oss) as the Town default, and gives the Mayor
   Claude. The same idea the other way around:

   gt config mix default=claude mayor=opencode

   Prefer one role at a time? Same result:

   gt config default-agent opencode
   gt config role set mayor claude

   Peek at the mix any time:

   gt config mix
   gt config role list

   A changed mix applies to new sessions. Restart or hand off a running
   Role to pick it up.

3. Start the Town

   gt up

4. Check that it is alive

   gt status

5. Add a project (a Rig)

   gt rig add myproject https://github.com/you/your-repo.git

That is the whole first hour. Come back here any time with gt hint.
`
}
