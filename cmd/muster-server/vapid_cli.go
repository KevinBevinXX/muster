package main

import (
	"fmt"
	"io"

	"github.com/ZacxDev/muster/internal/push"
)

// runVAPIDCLI is `muster-server vapid generate`: print a fresh VAPID keypair as
// the two environment lines muster reads, ready for an env file or a Secret.
//
// 🔴 THE PRIVATE KEY GOES TO STDOUT AND NOWHERE ELSE. Pipe it straight into the
// secret store (e.g. `… | kubectl create secret generic … --from-env-file=/dev/stdin
// --dry-run=client -o yaml | sops --encrypt …`); never paste it into a chat or a
// ticket. The subject is not generated — it is the operator's contact — so it
// is named on stderr rather than invented.
//
// ⚠ A NEW KEYPAIR STRANDS EVERY EXISTING SUBSCRIPTION. A browser subscription
// is bound to the public key it was made with; after a rotation each device
// must resubscribe (the client does so when it sees the key change).
func runVAPIDCLI(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 || args[0] != "generate" {
		fmt.Fprintln(stderr, "usage: muster-server vapid generate")
		return 2
	}
	priv, pub, err := push.GenerateVAPIDKeys()
	if err != nil {
		fmt.Fprintf(stderr, "vapid generate: %v\n", err)
		return 1
	}
	// Self-check the pair with the same validation boot applies, so a generator
	// defect cannot hand the operator a pair the server will refuse.
	if err := (push.VAPIDConfig{PublicKey: pub, PrivateKey: priv, Subject: "mailto:check@example.com"}).Validate(); err != nil {
		fmt.Fprintf(stderr, "vapid generate: the generated pair failed validation: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "%s=%s\n%s=%s\n", envVAPIDPublicKey, pub, envVAPIDPrivateKey, priv)
	fmt.Fprintf(stderr, "Also set %s to mailto:<your address> or an https URL.\n", envVAPIDSubject)
	return 0
}
