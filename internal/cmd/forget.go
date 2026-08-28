package cmd

import (
	"fmt"
	"strings"

	"github.com/jonbaldie/gastown/internal/style"
	"github.com/spf13/cobra"
)

func init() {
	cmd := newForgetCommand()
	cmd.GroupID = GroupWork
	rootCmd.AddCommand(cmd)
}

func newForgetCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "forget <key>",
		Short: "Remove a stored memory",
		Long: `Remove a memory from the beads key-value store.

The key should match the short name shown by 'gt memories'.
For typed memories, use type/key format or just the key (searches all types).

Examples:
  gt forget refinery-worktree
  gt forget feedback/dont-mock-db
  gt forget hooks-package-structure`,
		Args: cobra.ExactArgs(1),
		RunE: runForget,
	}
}

func runForget(_ *cobra.Command, args []string) error {
	key := args[0]
	// Strip memory. prefix if the user included it
	key = strings.TrimPrefix(key, memoryKeyPrefix)

	if handled, err := forgetTypedMemory(key); handled {
		return err
	}
	if handled, err := forgetOrderedMemory(key); handled {
		return err
	}
	return forgetLegacyMemory(key)
}

func forgetTypedMemory(key string) (bool, error) {
	// Support type/key format (e.g., "feedback/dont-mock-db").
	if slashIdx := strings.Index(key, "/"); slashIdx > 0 {
		memType := key[:slashIdx]
		shortKey := key[slashIdx+1:]
		if _, ok := validMemoryTypes[memType]; !ok {
			return false, nil
		}

		fullKey := memoryKeyPrefix + memType + "." + shortKey
		existing, err := bdKvGet(fullKey)
		if err != nil || existing == "" {
			return true, fmt.Errorf("memory %q not found", key)
		}
		if err := bdKvClear(fullKey); err != nil {
			return true, fmt.Errorf("removing memory: %w", err)
		}
		printForgotMemory(key)
		return true, nil
	}
	return false, nil
}

func forgetOrderedMemory(key string) (bool, error) {
	// Try typed key first (memory.<type>.<key> for each known type).
	for _, t := range memoryTypeOrder {
		fullKey := memoryKeyPrefix + t + "." + key
		existing, _ := bdKvGet(fullKey)
		if existing == "" {
			continue
		}
		if err := bdKvClear(fullKey); err != nil {
			return true, fmt.Errorf("removing memory: %w", err)
		}
		displayKey := key
		if t != "general" {
			displayKey = t + "/" + key
		}
		printForgotMemory(displayKey)
		return true, nil
	}
	return false, nil
}

func forgetLegacyMemory(key string) error {
	// Try legacy untyped key (memory.<key>).
	fullKey := memoryKeyPrefix + key
	existing, err := bdKvGet(fullKey)
	if err != nil || existing == "" {
		return fmt.Errorf("memory %q not found", key)
	}

	if err := bdKvClear(fullKey); err != nil {
		return fmt.Errorf("removing memory: %w", err)
	}

	printForgotMemory(key)
	return nil
}

func printForgotMemory(key string) {
	fmt.Printf("%s Forgot memory: %s\n", style.Success.Render("✓"), style.Bold.Render(key))
}
