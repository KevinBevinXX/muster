package main

import "github.com/ZacxDev/muster/internal/api"

// e2eMirrorStepUp, when non-nil, is the rendered view's step-up gate. Only a
// binary built with `-tags e2eterm` sets it (e2estepup.go); every shipped
// binary leaves it nil, and a nil gate refuses every send.
var e2eMirrorStepUp api.StepUpGate
