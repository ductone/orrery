package core

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ductone/orrey/internal/agentproto"
)

func TestValidateAttachmentsRejectsSymlinkAndDuplicate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(path, []byte("ok"), 0o600); err != nil {
		t.Fatal(err)
	}
	req := agentproto.TaskRequest{Attachments: []agentproto.AttachmentRef{{ID: "a-1", Path: path}}}
	if err := validateAttachments(&req); err != nil {
		t.Fatal(err)
	}
	req.Attachments = append(req.Attachments, req.Attachments[0])
	if err := validateAttachments(&req); err == nil {
		t.Fatal("duplicate attachment ID accepted")
	}
	link := filepath.Join(dir, "link.txt")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	req.Attachments = []agentproto.AttachmentRef{{ID: "a-2", Path: link}}
	if err := validateAttachments(&req); err == nil {
		t.Fatal("symlink attachment accepted")
	}
}
