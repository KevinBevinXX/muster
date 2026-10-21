package agentprovision

import "github.com/ZacxDev/muster/internal/agentprovision/scrubexec"

// 🔴 EVERY EXEC INTO AN AGENT'S CONTAINER STRIPS ITS TURN CREDENTIALS, BY
// STRUCTURE: a.driver is a scrubexec.Driver, built once in New from
// Config.Driver, and the raw driver is an unexported field of that type in
// another package — so nothing in this package can reach it, and every
// provision.Exec / provision.ExecTTY on a.driver runs `env -u …`
// (scrubexec.Command). TestTheRawDriverIsReachableOnlyThroughTheScrub pins the
// structure with go/types; TestNoExecInTheModuleBypassesTheScrub resolves every
// exec entry in the module's code.

// agentExecCommand is the prefix scrubexec puts on every command (tests name it).
var agentExecCommand = scrubexec.Command
