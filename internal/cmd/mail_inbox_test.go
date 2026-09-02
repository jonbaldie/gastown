package cmd

import (
	"errors"
	"testing"

	"github.com/jonbaldie/gastown/internal/mail"
)

type fakeInboxLister struct {
	calls    int
	messages []*mail.Message
	err      error
}

func (f *fakeInboxLister) List() ([]*mail.Message, error) {
	f.calls++
	return f.messages, f.err
}

func TestLoadInboxSnapshotListsOnceAndCounts(t *testing.T) {
	box := &fakeInboxLister{
		messages: []*mail.Message{
			{ID: "msg-1", Read: false},
			{ID: "msg-2", Read: true},
			{ID: "msg-3", Read: false},
		},
	}

	snapshot, err := loadInboxSnapshot(box, false)
	if err != nil {
		t.Fatalf("loadInboxSnapshot returned error: %v", err)
	}
	if box.calls != 1 {
		t.Fatalf("List calls = %d, want 1", box.calls)
	}
	if snapshot.total != 3 || snapshot.unread != 2 {
		t.Fatalf("counts = (%d total, %d unread), want (3, 2)", snapshot.total, snapshot.unread)
	}
	if len(snapshot.messages) != 3 {
		t.Fatalf("messages len = %d, want 3", len(snapshot.messages))
	}
}

func TestLoadInboxSnapshotUnreadOnlyFiltersAfterSingleList(t *testing.T) {
	box := &fakeInboxLister{
		messages: []*mail.Message{
			{ID: "msg-1", Read: false},
			{ID: "msg-2", Read: true},
			{ID: "msg-3", Read: false},
		},
	}

	snapshot, err := loadInboxSnapshot(box, true)
	if err != nil {
		t.Fatalf("loadInboxSnapshot returned error: %v", err)
	}
	if box.calls != 1 {
		t.Fatalf("List calls = %d, want 1", box.calls)
	}
	if snapshot.total != 3 || snapshot.unread != 2 {
		t.Fatalf("counts = (%d total, %d unread), want (3, 2)", snapshot.total, snapshot.unread)
	}
	if len(snapshot.messages) != 2 {
		t.Fatalf("filtered messages len = %d, want 2", len(snapshot.messages))
	}
	if snapshot.messages[0].ID != "msg-1" || snapshot.messages[1].ID != "msg-3" {
		t.Fatalf("filtered messages = [%s %s], want [msg-1 msg-3]", snapshot.messages[0].ID, snapshot.messages[1].ID)
	}
}

func TestLoadInboxSnapshotPropagatesListError(t *testing.T) {
	wantErr := errors.New("list failed")
	box := &fakeInboxLister{err: wantErr}

	_, err := loadInboxSnapshot(box, false)
	if !errors.Is(err, wantErr) {
		t.Fatalf("error = %v, want %v", err, wantErr)
	}
	if box.calls != 1 {
		t.Fatalf("List calls = %d, want 1", box.calls)
	}
}
