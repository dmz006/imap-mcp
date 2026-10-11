package unsubscribe

import (
	"time"
	_ "unsafe" // go:linkname

	_ "github.com/emersion/go-msgauth/dkim"
)

// dkimNow is go-msgauth's clock. It is only used to reject signatures past
// their x= expiry, before the cryptographic check, and the library has no
// option for it. An expired signature that otherwise verifies still shows the
// sender put the signed headers there (D58), so the expiry check is turned
// off here; Analyze reports expiry itself from Verification.Expiration.
//
//go:linkname dkimNow github.com/emersion/go-msgauth/dkim.now
var dkimNow func() time.Time

func init() { dkimNow = func() time.Time { return time.Unix(0, 0) } }
