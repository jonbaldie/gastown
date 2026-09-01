package cmd

import (
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/jonbaldie/gastown/internal/beads"
	"github.com/jonbaldie/gastown/internal/mail"
	"github.com/jonbaldie/gastown/internal/style"
	"github.com/jonbaldie/gastown/internal/workspace"
	"github.com/spf13/cobra"
)

// runMoleculeAttachFromMail handles the "gt mol attach-from-mail <mail-id>" command.
// It reads a mail message, extracts the molecule ID from the body, and attaches
// it to the current agent's hook (pinned bead).
func runMoleculeAttachFromMail(_ *cobra.Command, args []string) error {
	request, err := prepareMoleculeAttach(args[0])
	if err != nil {
		return err
	}

	issue, err := request.beads.AttachMolecule(request.hookBead.ID, request.moleculeID)
	if err != nil {
		return fmt.Errorf("attaching molecule: %w", err)
	}

	markMoleculeAttachMailRead(request.mailbox, request.mailID)
	printMoleculeAttachSuccess(request, issue)
	return nil
}

type moleculeAttachRequest struct {
	mailID        string
	agentIdentity string
	moleculeID    string
	mailbox       *mail.Mailbox
	beads         *beads.Beads
	hookBead      *beads.Issue
}

func prepareMoleculeAttach(mailID string) (*moleculeAttachRequest, error) {
	agentIdentity, err := moleculeAttachIdentity()
	if err != nil {
		return nil, err
	}

	mailbox, moleculeID, err := readMoleculeAttachMail(agentIdentity, mailID)
	if err != nil {
		return nil, err
	}

	b, err := moleculeAttachBeads()
	if err != nil {
		return nil, err
	}
	hookBead, err := findMoleculeAttachHook(b, agentIdentity)
	if err != nil {
		return nil, err
	}
	if err := verifyMoleculeAttachMolecule(b, moleculeID); err != nil {
		return nil, err
	}

	return &moleculeAttachRequest{
		mailID:        mailID,
		agentIdentity: agentIdentity,
		moleculeID:    moleculeID,
		mailbox:       mailbox,
		beads:         b,
		hookBead:      hookBead,
	}, nil
}

func moleculeAttachIdentity() (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("getting current directory: %w", err)
	}
	townRoot, err := workspace.FindFromCwd()
	if err != nil || townRoot == "" {
		return "", fmt.Errorf("not in a Gas Town workspace")
	}

	roleInfo, err := GetRoleWithContext(cwd, townRoot)
	if err != nil {
		return "", fmt.Errorf("detecting role: %w", err)
	}
	roleCtx := RoleContext{
		Role:     roleInfo.Role,
		Rig:      roleInfo.Rig,
		Polecat:  roleInfo.Polecat,
		TownRoot: townRoot,
		WorkDir:  cwd,
	}
	agentIdentity := buildAgentIdentity(roleCtx)
	if agentIdentity == "" {
		return "", fmt.Errorf("cannot determine agent identity (role: %s)", roleCtx.Role)
	}
	return agentIdentity, nil
}

func readMoleculeAttachMail(agentIdentity, mailID string) (*mail.Mailbox, string, error) {
	mailWorkDir, err := findMailWorkDir()
	if err != nil {
		return nil, "", fmt.Errorf("finding mail workspace: %w", err)
	}

	mailbox, err := mail.NewRouter(mailWorkDir).GetMailbox(agentIdentity)
	if err != nil {
		return nil, "", fmt.Errorf("getting mailbox: %w", err)
	}
	msg, err := mailbox.Get(mailID)
	if err != nil {
		return nil, "", fmt.Errorf("reading mail message: %w", err)
	}
	moleculeID := extractMoleculeIDFromMail(msg.Body)
	if moleculeID == "" {
		return nil, "", fmt.Errorf("no attached_molecule field found in mail body")
	}
	return mailbox, moleculeID, nil
}

func moleculeAttachBeads() (*beads.Beads, error) {
	workDir, err := findLocalBeadsDir()
	if err != nil {
		return nil, fmt.Errorf("not in a beads workspace: %w", err)
	}
	return beads.New(workDir), nil
}

func findMoleculeAttachHook(b *beads.Beads, agentIdentity string) (*beads.Issue, error) {
	pinnedBeads, err := b.List(beads.ListOptions{
		Status:   beads.StatusPinned,
		Assignee: agentIdentity,
		Priority: -1,
	})
	if err != nil {
		return nil, fmt.Errorf("listing pinned beads: %w", err)
	}
	if len(pinnedBeads) == 0 {
		return nil, fmt.Errorf("no pinned bead found for agent %s - create one first", agentIdentity)
	}
	return pinnedBeads[0], nil
}

func verifyMoleculeAttachMolecule(b *beads.Beads, moleculeID string) error {
	if _, err := b.Show(moleculeID); err != nil {
		return fmt.Errorf("molecule %s not found: %w", moleculeID, err)
	}
	return nil
}

func markMoleculeAttachMailRead(mailbox *mail.Mailbox, mailID string) {
	if err := mailbox.MarkRead(mailID); err != nil {
		// Non-fatal: log warning but don't fail.
		style.PrintWarning("could not mark mail as read: %v", err)
	}
}

func printMoleculeAttachSuccess(request *moleculeAttachRequest, issue *beads.Issue) {
	attachment := beads.ParseAttachmentFields(issue)
	fmt.Printf("%s Attached molecule from mail\n", style.Bold.Render("✓"))
	fmt.Printf("  Mail: %s\n", request.mailID)
	fmt.Printf("  Hook: %s\n", request.hookBead.ID)
	fmt.Printf("  Molecule: %s\n", request.moleculeID)
	if attachment != nil && attachment.AttachedAt != "" {
		fmt.Printf("  Attached at: %s\n", attachment.AttachedAt)
	}
	fmt.Printf("\n%s Run 'gt hook' to see progress\n", style.Dim.Render("Hint:"))
}

// extractMoleculeIDFromMail extracts a molecule ID from a mail message body.
// It looks for patterns like:
//   - attached_molecule: <id>
//   - molecule_id: <id>
//   - molecule: <id>
//
// The ID is expected to be on the same line after the colon.
func extractMoleculeIDFromMail(body string) string {
	// Try various patterns for molecule ID in mail body (case-insensitive)
	patterns := []string{
		`(?i)attached_molecule:\s*(\S+)`,
		`(?i)molecule_id:\s*(\S+)`,
		`(?i)molecule:\s*(\S+)`,
		`(?i)mol:\s*(\S+)`,
	}

	for _, pattern := range patterns {
		re := regexp.MustCompile(pattern)
		matches := re.FindStringSubmatch(body)
		if len(matches) >= 2 {
			return strings.TrimSpace(matches[1])
		}
	}

	return ""
}
