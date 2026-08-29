package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/jonbaldie/gastown/internal/crew"
	"github.com/jonbaldie/gastown/internal/git"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/rig"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/tmux"
	"github.com/spf13/cobra"
)

// CrewStatusItem represents detailed status for a crew worker.
type CrewStatusItem struct {
	Name         string   `json:"name"`
	Rig          string   `json:"rig"`
	Path         string   `json:"path"`
	Branch       string   `json:"branch"`
	HasSession   bool     `json:"has_session"`
	SessionID    string   `json:"session_id,omitempty"`
	GitClean     bool     `json:"git_clean"`
	GitModified  []string `json:"git_modified,omitempty"`
	GitUntracked []string `json:"git_untracked,omitempty"`
	MailTotal    int      `json:"mail_total"`
	MailUnread   int      `json:"mail_unread"`
}

func runCrewStatus(_ *cobra.Command, args []string) error {
	state := crewState()
	targetName := resolveCrewStatusTarget(args)
	t := tmux.NewTmux()
	items, err := collectCrewStatusItems(targetName, t)
	if err != nil {
		return err
	}

	if len(items) == 0 {
		fmt.Println("No crew workspaces found.")
		return nil
	}

	if state.json {
		return printCrewStatusJSON(items)
	}

	printCrewStatusText(items)
	return nil
}

func resolveCrewStatusTarget(args []string) string {
	state := crewState()
	if len(args) == 0 {
		return ""
	}

	targetName := args[0]
	if rig, crewName, ok := parseRigSlashName(targetName); ok {
		if state.rig == "" {
			state.rig = rig
		}
		return crewName
	}
	if state.rig == "" {
		// A single rig name means show status for all crew in that rig.
		if _, _, err := getRig(targetName); err == nil {
			state.rig = targetName
			return ""
		}
	}
	return targetName
}

func collectCrewStatusItems(targetName string, t *tmux.Tmux) ([]CrewStatusItem, error) {
	if targetName == "" && crewState().rig == "" {
		return collectAllRigsCrewStatus(t)
	}
	return collectOneRigCrewStatus(targetName, t)
}

func collectAllRigsCrewStatus(t *tmux.Tmux) ([]CrewStatusItem, error) {
	rigs, err := getAllRigs()
	if err != nil {
		return nil, err
	}

	var items []CrewStatusItem
	for _, r := range rigs {
		rigItems, err := listCrewStatusItems(r, t)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: failed to list crew workers in %s: %v\n", r.Name, err)
			continue
		}
		items = append(items, rigItems...)
	}
	return items, nil
}

func collectOneRigCrewStatus(targetName string, t *tmux.Tmux) ([]CrewStatusItem, error) {
	crewMgr, r, err := getCrewManagerForMember(crewState().rig, targetName)
	if err != nil {
		return nil, err
	}
	workers, err := crewStatusWorkers(crewMgr, targetName)
	if err != nil {
		return nil, err
	}
	return buildCrewStatusItems(r, workers, t), nil
}

func crewStatusWorkers(crewMgr *crew.Manager, targetName string) ([]*crew.CrewWorker, error) {
	if targetName == "" {
		workers, err := crewMgr.List()
		if err != nil {
			return nil, fmt.Errorf("listing crew workers: %w", err)
		}
		return workers, nil
	}

	worker, err := crewMgr.Get(targetName)
	if err != nil {
		if err == crew.ErrCrewNotFound {
			return nil, fmt.Errorf("crew workspace '%s' not found", targetName)
		}
		return nil, fmt.Errorf("getting crew worker: %w", err)
	}
	return []*crew.CrewWorker{worker}, nil
}

func printCrewStatusJSON(items []CrewStatusItem) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(items)
}

func printCrewStatusText(items []CrewStatusItem) {
	for i, item := range items {
		if i > 0 {
			fmt.Println()
		}
		printCrewStatusItem(item)
	}
}

func printCrewStatusItem(item CrewStatusItem) {
	sessionStatus := style.Dim.Render("○ stopped")
	if item.HasSession {
		sessionStatus = style.Bold.Render("● running")
	}

	fmt.Printf("%s %s/%s\n", sessionStatus, item.Rig, item.Name)
	fmt.Printf("  Path:   %s\n", item.Path)
	fmt.Printf("  Branch: %s\n", item.Branch)
	printCrewGitStatus(item)
	printCrewMailStatus(item)
}

func printCrewGitStatus(item CrewStatusItem) {
	if item.GitClean {
		fmt.Printf("  Git:    %s\n", style.Dim.Render("clean"))
		return
	}

	fmt.Printf("  Git:    %s\n", style.Bold.Render("dirty"))
	if len(item.GitModified) > 0 {
		fmt.Printf("          Modified: %s\n", strings.Join(item.GitModified, ", "))
	}
	if len(item.GitUntracked) > 0 {
		fmt.Printf("          Untracked: %s\n", strings.Join(item.GitUntracked, ", "))
	}
}

func printCrewMailStatus(item CrewStatusItem) {
	if item.MailUnread > 0 {
		fmt.Printf("  Mail:   %d unread / %d total\n", item.MailUnread, item.MailTotal)
		return
	}
	fmt.Printf("  Mail:   %s\n", style.Dim.Render(fmt.Sprintf("%d messages", item.MailTotal)))
}

func listCrewStatusItems(r *rig.Rig, t *tmux.Tmux) ([]CrewStatusItem, error) {
	crewMgr := crew.NewManager(r, git.NewGit(r.Path))
	workers, err := crewMgr.List()
	if err != nil {
		return nil, fmt.Errorf("listing crew workers: %w", err)
	}
	return buildCrewStatusItems(r, workers, t), nil
}

func buildCrewStatusItems(r *rig.Rig, workers []*crew.CrewWorker, t *tmux.Tmux) []CrewStatusItem {
	items := make([]CrewStatusItem, 0, len(workers))

	for _, w := range workers {
		sessionID := crewSessionName(r.Name, w.Name)
		hasSession, _ := t.HasSession(sessionID)

		// Git status
		crewGit := git.NewGit(w.ClonePath)
		gitStatus, _ := crewGit.Status()
		branch, _ := crewGit.CurrentBranch()

		gitClean := true
		var modified, untracked []string
		if gitStatus != nil {
			gitClean = gitStatus.Clean
			modified = append(gitStatus.Modified, gitStatus.Added...)
			modified = append(modified, gitStatus.Deleted...)
			untracked = gitStatus.Untracked
		}

		// Mail status (non-fatal: display defaults to 0 if count fails)
		mailDir := filepath.Join(w.ClonePath, "mail")
		mailTotal, mailUnread := 0, 0
		if _, err := os.Stat(mailDir); err == nil {
			mailbox := mail.NewMailbox(mailDir)
			mailTotal, mailUnread, _ = mailbox.Count()
		}

		item := CrewStatusItem{
			Name:         w.Name,
			Rig:          r.Name,
			Path:         w.ClonePath,
			Branch:       branch,
			HasSession:   hasSession,
			GitClean:     gitClean,
			GitModified:  modified,
			GitUntracked: untracked,
			MailTotal:    mailTotal,
			MailUnread:   mailUnread,
		}
		if hasSession {
			item.SessionID = sessionID
		}

		items = append(items, item)
	}

	return items
}
