package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

var mailDirectoryCmd = &cobra.Command{
	Use:     "directory",
	Aliases: []string{"dir", "addresses"},
	Short:   "List all valid mail recipient addresses",
	Long: `List all valid mail recipient addresses in the town.

Shows agent addresses, group addresses, queue addresses, channel addresses,
and well-known special addresses.

Examples:
  gt mail directory              # List all addresses
  gt mail directory --json       # JSON output`,
	Args: cobra.NoArgs,
	RunE: runMailDirectory,
}

// DirectoryEntry represents an address in the directory.
type DirectoryEntry struct {
	Address string `json:"address"`
	Type    string `json:"type"`
}

func init() {
	mailDirectoryCmd.Flags().Bool("json", false, "Output as JSON")
	mailCmd.AddCommand(mailDirectoryCmd)
}

func runMailDirectory(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("not in a Gas Town workspace: %w", err)
	}

	entries, warnings := collectMailDirectoryEntries(beads.New(townRoot))

	if mailBoolFlag(cmd, "json") {
		return writeMailDirectoryJSON(entries)
	}
	return writeMailDirectoryText(entries, warnings)
}

func collectMailDirectoryEntries(b *beads.Beads) ([]DirectoryEntry, int) {
	var entries []DirectoryEntry
	warnings := 0

	entries, warnings = appendAgentDirectoryEntries(b, entries, warnings)
	entries, warnings = appendGroupDirectoryEntries(b, entries, warnings)
	entries, warnings = appendQueueDirectoryEntries(b, entries, warnings)
	entries, warnings = appendChannelDirectoryEntries(b, entries, warnings)
	entries = append(entries, []DirectoryEntry{
		{Address: "mayor/", Type: "well-known"},
		{Address: "--human", Type: "well-known"},
		{Address: "--self", Type: "well-known"},
		{Address: "@town", Type: "special"},
		{Address: "@crew", Type: "special"},
		{Address: "@witnesses", Type: "special"},
		{Address: "@overseer", Type: "special"},
	}...)

	entries = deduplicateDirectoryEntries(entries)
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Type != entries[j].Type {
			return entries[i].Type < entries[j].Type
		}
		return entries[i].Address < entries[j].Address
	})
	return entries, warnings
}

func appendAgentDirectoryEntries(b *beads.Beads, entries []DirectoryEntry, warnings int) ([]DirectoryEntry, int) {
	agents, err := b.ListAgentBeads()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list agents: %v\n", err)
		return entries, warnings + 1
	}
	for id := range agents {
		addr := mail.AgentBeadIDToAddress(id)
		if addr != "" {
			entries = append(entries, DirectoryEntry{Address: addr, Type: "agent"})
		}
	}
	return entries, warnings
}

func appendGroupDirectoryEntries(b *beads.Beads, entries []DirectoryEntry, warnings int) ([]DirectoryEntry, int) {
	groups, err := b.ListGroupBeads()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list groups: %v\n", err)
		return entries, warnings + 1
	}
	for name := range groups {
		entries = append(entries, DirectoryEntry{Address: "group:" + name, Type: "group"})
	}
	return entries, warnings
}

func appendQueueDirectoryEntries(b *beads.Beads, entries []DirectoryEntry, warnings int) ([]DirectoryEntry, int) {
	queues, err := b.ListQueueBeads()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list queues: %v\n", err)
		return entries, warnings + 1
	}
	for id, issue := range queues {
		if issue == nil {
			continue
		}
		fields := beads.ParseQueueFields(issue.Description)
		if fields.Name == "" {
			fmt.Fprintf(os.Stderr, "warning: queue %s has no name field, skipping\n", id)
			continue
		}
		entries = append(entries, DirectoryEntry{Address: "queue:" + fields.Name, Type: "queue"})
	}
	return entries, warnings
}

func appendChannelDirectoryEntries(b *beads.Beads, entries []DirectoryEntry, warnings int) ([]DirectoryEntry, int) {
	channels, err := b.ListChannelBeads()
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not list channels: %v\n", err)
		return entries, warnings + 1
	}
	for name := range channels {
		entries = append(entries, DirectoryEntry{Address: "channel:" + name, Type: "channel"})
	}
	return entries, warnings
}

func deduplicateDirectoryEntries(entries []DirectoryEntry) []DirectoryEntry {
	seen := make(map[string]bool)
	deduped := entries[:0]
	for _, entry := range entries {
		if !seen[entry.Address] {
			seen[entry.Address] = true
			deduped = append(deduped, entry)
		}
	}
	return deduped
}

func writeMailDirectoryJSON(entries []DirectoryEntry) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(entries)
}

func writeMailDirectoryText(entries []DirectoryEntry, warnings int) error {
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ADDRESS\tTYPE")
	for _, e := range entries {
		fmt.Fprintf(w, "%s\t%s\n", e.Address, e.Type)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if warnings > 0 {
		fmt.Fprintf(os.Stdout, "\nListed %d addresses (%d warnings)\n", len(entries), warnings)
	} else {
		fmt.Fprintf(os.Stdout, "\nListed %d addresses\n", len(entries))
	}
	return nil
}
