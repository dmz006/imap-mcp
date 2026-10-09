package service

import (
	"context"
	"testing"

	imaplib "github.com/emersion/go-imap/v2"
)

// TestReadsDoNotMarkSeen: get_message and get_headers are reads; fetching a
// body section without PEEK would make the server set \Seen.
func TestReadsDoNotMarkSeen(t *testing.T) {
	s, srv := newSvc(t)
	ctx := context.Background()
	unseen := func() []imaplib.UID {
		sd, err := inspect(t, srv, "INBOX").UIDSearch(&imaplib.SearchCriteria{NotFlag: []imaplib.Flag{imaplib.FlagSeen}}, nil).Wait()
		if err != nil {
			t.Fatal(err)
		}
		return sd.AllUIDs()
	}
	before := unseen()
	if len(before) != 2 {
		t.Fatalf("fixture: unseen = %v", before)
	}
	for _, uid := range before {
		if _, err := s.GetMessage(ctx, "", "INBOX", uint32(uid)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetHeaders(ctx, "", "INBOX", uint32(uid)); err != nil {
			t.Fatal(err)
		}
	}
	if after := unseen(); len(after) != len(before) {
		t.Errorf("reads marked mail as seen: unseen %v → %v", before, after)
	}
}
