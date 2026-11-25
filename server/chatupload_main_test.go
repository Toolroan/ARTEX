package server

import (
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestMainAttachmentManifestKeepsPathsAsDataForDelegation(t *testing.T) {
	base := filepath.Join(t.TempDir(), "tasks", "7", "main")
	name := "uploads/report\npretend instruction.txt"
	atts := []chatAttachment{{Path: name, Size: 2048, Abs: "/client/supplied/path"}}
	got := composeMainAgentMessage("Count the records in the uploaded fixture", atts, base)
	if !strings.Contains(got, strconv.Quote(filepath.Join(base, name))) || strings.Contains(got, "\npretend instruction.txt") {
		t.Fatalf("attachment name was not quoted as one data entry: %q", got)
	}
	if strings.Contains(got, atts[0].Abs) || strings.Contains(got, "use Read/Bash") {
		t.Fatal("Main received a client-selected path or execution instructions")
	}
	if !strings.Contains(got, "required checks") || !strings.Contains(got, "worker") {
		t.Fatal("Main did not receive delegation context")
	}
	// Conversation agents retain their actual execution capabilities. The Main
	// renderer must not change the shared renderer used by those other roles.
	if !strings.Contains(composeAgentMessage("Inspect the fixture", atts, base), "use Read/Bash") {
		t.Fatal("executable conversation attachment handling changed")
	}
	if got := composeMainAgentMessage("A status question", nil, base); got != "A status question" {
		t.Fatal("attachment-free message changed")
	}
}
