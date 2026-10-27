package main

// ---------------------------------------------------------------------------
// SEAMS THIS BINARY OPENS, AND WHAT CLOSES EACH ONE.
//
// This file declares nothing. It follows internal/api/doc_seams.go's shape
// because a seam recorded in a commit message is a seam nobody finds, and each
// entry names WHAT, WHY, the CLOSING CONDITION and WHO CHECKS IT — the four
// things that separate a work item from an object nobody can close.
//
// 🔴 THE DISTINCTION api.Extensions.defects DRAWS GOVERNS EVERY ENTRY HERE:
// "degrades visibly" versus "lies silently". A nil GitHub store leaves the Repos
// tab undrawn, which is TRUE — this server has no GitHub connection. A notes
// store with no session-liveness probe renders "no transcript recorded" over
// transcripts that may be alive, with nothing logged and nothing 404ing. The
// first is a configuration; the second is a page that states a falsehood, and
// /readyz refuses to serve with it. Each nil below is argued on that line, not
// on convenience.
//
// ---------------------------------------------------------------------------
// 1. 🔴 api.Provisioner IS NIL *UNLESS* MUSTER_AGENT_PROVISIONER NAMES A DRIVER,
//    AND api.Gateway IS NIL UNLESS MUSTER_AGENT_GATEWAY NAMES A RUNTIME. BOTH
//    HALVES OF THIS SEAM NOW HAVE AN IMPLEMENTATION; NEITHER IS ON BY DEFAULT.
//
//	✅ CURRENT STATE, READ THIS BEFORE ANYTHING BELOW — the rest of this entry is
//	  history kept because its argument is what the wiring is built on, and several
//	  paragraphs in it are now marked [WAS]. A KICKOFF IS DELIVERED: with
//	  MUSTER_AGENT_GATEWAY naming a runtime, buildKickoffDeliverer builds
//	  internal/agentkickoff over the same adapter and gateway, and
//	  startBackgroundLoops runs it. Every agentkickoff.DefaultInterval it drives
//	  agents.DecideReconcile over rows this deployment OWNS (stored namespace ==
//	  the configured prefix + name) that hold a pending note and were NEVER kicked
//	  off; for a ready instance it takes the per-agent claim
//	  (agents.Store.ClaimKickoff), re-reads the row, stamps KickedOff and the
//	  recipient BEFORE the turn, and sends the note through the gateway into the
//	  agent's latest chat session — Resolve (address + bearer, no connection to the
//	  runtime) before the stamp, Send after it; together they are Gateway.Chat. A failed or EMPTY turn is recorded in
//	  agents.kickoff_error and never re-run — and (PR #37 review round 0) SURFACED:
//	  agents.KickoffFailed drives a "kickoff failed" card badge with the scrubbed
//	  error text and a remedy (PR #37 round 1: "re-send" only when nothing was sent,
//	  otherwise "check whether the agent is already working first"), and `kickoffFailed` on
//	  GET /api/agents; a turn cut off by shutdown is recorded as such and
//	  app.shutdown waits (bounded) for that record before the pool closes. A
//	  pre-send failure is retried until
//	  agents.ProvisioningStuckTimeout, then the row is errored with the last send
//	  failure appended. agentprovision.KickoffDeliveryWired is true, so a dispatch
//	  is refused ONLY when no gateway is named.
//	  NOT DELIVERED BY IT, deliberately: re-sends to a kicked-off row whose
//	  recipient died (agents.ActionResendKickoff / ActionErrorKickoffLost) — the
//	  deliverer never acts on a kicked-off row, so that half of the table still has
//	  no driver.
//	  NOT VERIFIED IN-REPO: a delivered first turn against a muster-provisioned agent
//	  on a real cluster. internal/agentkickoff's seam test runs the real adapter,
//	  deliverer and gateway against a fake runtime over HTTP; the cluster turn is the
//	  closing condition at the foot of this entry.
//
//	WHAT CHANGED, AND WHAT ITS MECHANICAL SIGNAL WAS: internal/agentprovision
//	  adapts provision.Provisioner to api.Provisioner's seven LIFECYCLE methods,
//	  and provisioner.go constructs a driver behind MUSTER_AGENT_PROVISIONER
//	  (none | noop | kubernetes). The closing condition this entry set was that
//	  internal/provision/k8s LEAVE the not-linked ledger in
//	  internal/modulegate/linkage_test.go — it has, together with
//	  internal/agentspec, and that ledger is ASSERTED so neither could be removed
//	  from it without something really linking.
//	✅ THE CHAT HALF NOW HAS AN IMPLEMENTATION, AND THE PARAGRAPH HERE THAT SAID
//	  OTHERWISE IS REPLACED RATHER THAN LEFT TO READ AS OPEN. It said: "WHAT IS
//	  STILL NIL, AND IT IS NOT A DETAIL: api.Gateway. Nothing in this module
//	  implements a model gateway". internal/agentgateway does, over the SAME driver
//	  the lifecycle adapter holds, behind MUSTER_AGENT_GATEWAY (none | hooks-sha256)
//	  plus MUSTER_AGENT_GATEWAY_MODEL. Naming a runtime with no driver is refused at
//	  boot, because this binary's only source of an instance's address is a driver.
//	🔴 [WAS, until internal/agentkickoff] WHAT THAT DOES *NOT* CLOSE, AND IT IS
//	  THE HALF THAT MATTERS FOR A KICKOFF: NOTHING CALLS THE GATEWAY ON THE DISPATCH
//	  PATH. The two CHAT ROUTES stop
//	  REFUSING — which is not the same as reachable, see below; the kickoff is not a
//	  route at all. A dispatch with kickoff=true still CREATES the
//	  instance and does not deliver the first message — the note stays in
//	  agents.pending_note and the non-delivery is written to agents.kickoff_error
//	  (agentprovision.UndeliveredKickoffReason). An agent dispatched on such a
//	  deployment is still a real pod that was never told what to do.
//	🔴 THE PARAGRAPH ABOVE IS NOW TRUE ONLY WHERE MUSTER_AGENT_GATEWAY *NAMES* A
//	  RUNTIME, AND IT IS CORRECTED HERE RATHER THAN REWRITTEN BECAUSE THE SENTENCE
//	  "still CREATES the instance" IS WHAT agentprovision.KickoffRefusalReason CITES
//	  THIS ENTRY FOR. With the variable UNSET — which is every deployment running
//	  today — such a dispatch is now REFUSED instead: agentprovision.Adapter.Dispatch
//	  creates nothing, mints no token, marks the row `error` with the cause and the
//	  remedy, and returns agentprovision.ErrKickoffUndeliverable.
//	  🔴 [WAS, until internal/agentkickoff] AND THAT REFUSAL NOW FIRES ON *EVERY*
//	  DEPLOYMENT, NOT ONLY THE UNSET ONE.
//	  THIS PARAGRAPH SAID "buildAgentPlane passes `gw != nil` into the adapter, so the
//	  refusal keys on the RESOLVED configuration and a deployment that has named a
//	  runtime is unchanged", AND THAT WAS THE DEFECT RATHER THAN THE DESIGN. `gw !=
//	  nil` is the NECESSARY half of deliverability and not the sufficient one: the
//	  paragraph two above says in as many words that NOTHING CALLS THE GATEWAY ON THE
//	  DISPATCH PATH, so a named runtime bought a gateway nothing invokes. Measured on
//	  the deployment running today, which HAS named one: the adapter was told a
//	  kickoff was deliverable, the refusal was skipped, and the click produced exactly
//	  the pod-that-was-never-told-what-to-do this entry describes. buildAgentPlane now
//	  passes kickoffDeliverable(gw, agentprovision.KickoffDeliveryWired) — both
//	  conjuncts — and internal/modulegate's
//	  TestKickoffDeliveryLedgerAgreesWithTheModule fails when that constant and the
//	  module's own call graph disagree, in either direction, so the call site named in
//	  the CLOSING CONDITION below cannot land without flipping it.
//	  ⚠ THE REFUSAL IS ON THE CREATE-WITH-KICKOFF PATH ONLY. Start is deliberately
//	  still allowed and, with no gateway, still records the non-delivery in
//	  agents.kickoff_error, where it is honest because an instance exists (with a
//	  gateway it records nothing and the deliverer pays the turn); refusing it would have removed
//	  restart, eviction-recovery and the save-then-start-later route this entry's own
//	  Dispatch doc calls the supported one. See Adapter.Start.
//	  ✅ THE REFUSAL REACHES THE HTTP RESPONSE NOW, AND THE CLOSING CONDITION THAT
//	  ASKED FOR IT IS KEPT HERE WITH ITS OUTCOME. It read: "WHAT IT DOES *NOT* FIX …
//	  the refusal does not reach the HTTP RESPONSE of the POST that asked.
//	  internal/api's createAndDispatchAgent calls Dispatch inside safeGo AFTER
//	  creating the row, so the operator's POST has already answered 200 … CLOSING
//	  CONDITION: a synchronous check in internal/api's create handler, before the row
//	  exists, keyed on the same capability, answering 4xx with
//	  agentprovision.KickoffRefusalReason's text." handleAgentCreate asks
//	  api.Provisioner.KickoffUndeliverableReason — the same field Dispatch branches on,
//	  which is what stops the two answers disagreeing — and answers 409 carrying it
//	  before Agents.Create. The async half of the sentence is still TRUE of Dispatch
//	  itself, which is why the capability is a separate question rather than a
//	  different return value.
//	  ⚠ ONE CALLER IS STILL ASYNCHRONOUS, DELIBERATELY: handleChiefProvision
//	  reaches Dispatch through createAndDispatchAgent inside safeGo, so its refusal
//	  is recorded on the row (`error` + the reason) rather than returned. (Runbook
//	  dispatch was the second such caller until runbooks were removed.) It has no
//	  "Save for later" affordance for a 4xx to point at; the row they
//	  create can be brought up with Start, which is the same escape by another door.
//	  ⚠ AND THE UI STILL *OFFERS* THE DISPATCH BUTTON ON A DEPLOYMENT THAT WILL REFUSE
//	  IT. The refusal is legible once clicked — internal/ui's htmx:responseError
//	  handler toasts the 409 body verbatim — but nothing dims the affordance, so the
//	  operator learns by being refused. CLOSING CONDITION: a pull request in which the
//	  dispatch modal's Dispatch control is disabled, with the reason beside it, when
//	  the provisioner reports a kickoff undeliverable. WHO CHECKS IT: the reviewer of
//	  that pull request, against a rendered modal on a deployment with
//	  agentprovision.KickoffDeliveryWired false.
//	🔴 AND "A HUMAN CAN NOW OPEN ITS CHAT AND TALK TO IT" WAS FALSE FOR AN AGENT
//	  *THIS BINARY* PROVISIONED — that sentence stood here and an audit measured it.
//	  TWO things block it, and both are OUTSIDE internal/agentgateway:
//
//	  ✅ BOTH ARE CLOSED NOW, AND WHAT REPLACES THEM IS A WEAKER CLAIM RATHER THAN
//	  NOTHING — read to the end of (2) before concluding a turn works. Each is kept
//	  in its original wording with its outcome appended, because both closing
//	  conditions were written here and the only way to check they were MET is to
//	  read what they asked for.
//
//	  (1) NO ADDRESS. [WAS] agentspec.Build renders provision.Spec with Ports nil
//	      and Endpoint nil (its committed golden says so), so k8s render creates NO
//	      Service and driver.Endpoint answers provision.ErrNoEndpoint. Every chat
//	      turn against such an agent fails per-turn — worse than the 503 it replaced,
//	      which at least named its own cause.
//	      CLOSING CONDITION: agentspec.Build declares a port (and the driver renders
//	      a Service), verified by a test that resolves an endpoint through a REAL
//	      driver — a fake clientset is enough — rather than through a stub resolver.
//	      Every test in this module today uses a stub, which is exactly why this
//	      shipped: both sides were tested and the SEAM was not.
//	      ✅ MET. agentspec.Build declares one port named provision.DefaultPortName,
//	      from agentspec.DefaultGatewayPort or MUSTER_AGENT_GATEWAY_PORT, so
//	      renderService produces a Service and renderAnnotations writes the port the
//	      driver resolves from. NO change to internal/provision/k8s was needed — the
//	      driver already rendered container ports, wrote the annotation and read it
//	      back; the whole defect was the spec. The test the condition asked for is
//	      TestAnAgentThisBinaryProvisionsResolvesAnEndpoint, in THIS package rather
//	      than in either package it exercises: internal/agentspec's suite has no
//	      cluster client and internal/provision/k8s's has no agent row, so neither
//	      could hold the combined state. Mutating buildPorts back to returning nil
//	      reproduces the exact string above — `declares no port`.
//	      ⚠ AN ALREADY-PROVISIONED INSTANCE DOES *NOT* GAIN THE PORT BY ITSELF, AND
//	      NO MIGRATION IS OWED — the two halves of that sentence are independent and
//	      both were checked. The mechanism is real: Driver.Endpoint reads the
//	      `muster.dev/port` annotation written on the DEPLOYMENT at CREATE time (see
//	      k8s.AnnotationPort), so an
//	      instance created before this change carries none and resolving its address
//	      would still fail. What makes it moot is that the victim set is EMPTY —
//	      measured live with controls: zero namespaces under the configured prefix
//	      (positive control: the upstream project's own namespaces were found, so the
//	      query worked) and zero objects labelled managed-by muster (positive
//	      control: objects managed by the upstream installer were found). muster has
//	      provisioned nothing, so there is nothing holding a stale annotation, and the
//	      next Start on any row takes the Create path and gets the port. Written down
//	      so the next reader does not re-derive the mechanism and conclude a migration
//	      is needed; if this binary ever provisions before a change of this shape, it
//	      will be.
//	  (2) NO CREDENTIAL UNDER THE NAME THE IMAGE READS. [WAS] The gateway bearer is
//	      sha256("gw-" + HOOKS_TOKEN) — the agent CONTAINER's variable — while
//	      agentspec ships the row's token as MUSTER_HOOK_TOKEN (agentspec.EnvToken)
//	      and nothing bridges the two. Measured against a live runtime provisioned by
//	      the upstream service, which ships HOOKS_TOKEN: that derivation is accepted
//	      (200, and a deliberately wrong bearer is refused 401). So an agent THIS
//	      binary provisions would 401 on every turn — 🔴 ONCE (1) IS FIXED, AND NOT
//	      BEFORE. The two blockers are ORDERED: Gateway.reach resolves the endpoint
//	      before it builds a request, so while (1) holds the observable is
//	      ErrNoEndpoint and NO 401 is reachable. An earlier revision of this line
//	      predicted the 401 unconditionally, which sends a debugger hunting a
//	      credential when the first failure is address resolution.
//	      CLOSING CONDITION: the provisioned container receives the token under the
//	      name its gateway derives from, proven by one real turn against an instance
//	      THIS binary created — not one created by the upstream service.
//	      ✅ THE FIRST HALF IS MET AND 🔴 THE PROOF IS NOT. agentspec.buildSecrets
//	      now emits the row's token under agentspec.EnvGatewayToken as well as
//	      EnvToken, from ONE branch so they cannot drift apart — a SECOND NAME and
//	      not a rename, because EnvToken is what cmd/muster's in-pod CLI reads and
//	      what the autosave daemon posts its durability alarm with, so renaming
//	      would have traded a chat 401 for a silent agent.
//	      TestTheProvisionedContainerCanDeriveTheBearerMusterSends reproduces the
//	      container's documented derivation over the built spec's own environment and
//	      requires the two bearers to match; it dies to a wrong name, a wrong value
//	      and an empty value, each of which has the same runtime symptom.
//	      🔴 THE OWED MEASUREMENT WAS PARTLY TAKEN, AND IT FOUND A THIRD BLOCKER
//	      THAT NEITHER OF THESE TWO PREDICTED. This paragraph used to read: "WHAT IS
//	      STILL OWED IS THE SENTENCE THIS CONDITION ACTUALLY WROTE: 'ONE REAL TURN
//	      AGAINST AN INSTANCE THIS BINARY CREATED'. No such turn has been made." A
//	      muster-provisioned agent was then created on a live cluster, and it never
//	      got as far as a 401 OR a 200: the pod CRASHLOOPED. exitCode 78, seven
//	      restarts, 0/1 for ever, with
//
//	        [gateway] loading configuration…
//	        [gateway] resolving authentication…
//	        Missing config. Run `<setup>` or set gateway.mode=local (or pass --allow-unconfigured).
//
//	      ✅ THE BEARER FORMULA IS CONFIRMED CORRECT, AND THAT IS THE HALF THIS
//	      ENTRY ASKED ABOUT. Reproduced locally against the same image with the
//	      configuration installed: a request carrying sha256("gw-" + HOOKS_TOKEN) is
//	      ACCEPTED — it reaches the route and is refused only on the model field
//	      (HTTP 400, `Invalid model`) — while the identical request with a wrong
//	      bearer is 401 and with no bearer is 401. agentgateway.HooksSHA256 was never
//	      the defect.
//	      🔴 WHAT *WAS* MISSING IS CONFIGURATION INSTALLATION, WHICH IS A DIFFERENT
//	      BLOCKER AND IS NOW CLOSED ON ITS MECHANISM. Three measured facts, each of
//	      which rules out an obvious fix: the image's own entrypoint already starts
//	      its gateway, so `command: null` was never the fault; starting it with the
//	      unconfigured escape hatch reaches ready and serves 200 on `/` while the
//	      model-response route answers 404 for EVERY bearer (none, wrong and correct
//	      alike — a 404 attributes nothing, so all three were measured side by side);
//	      and what registers that route is a key in the runtime's own configuration
//	      FILE, which is also the only place a gateway credential is read from.
//	      internal/agentspec/runtimeconfig.go installs an OPERATOR-SUPPLIED bundle
//	      (MUSTER_AGENT_RUNTIME_CONFIG + MUSTER_AGENT_RUNTIME_INSTALL, both required
//	      whenever a runtime is named and both refused at boot) and ships the DERIVED
//	      bearer as agentspec.EnvGatewayBearer, so there is one implementation of the
//	      formula rather than one here and one in shell. That file carries the (A)/(B)
//	      design argument and every measurement above.
//	      🔴 AND A CRASHLOOP IS NOW VISIBLE, WHICH IT WAS NOT. This driver rendered no
//	      probe of any kind, so the crashlooping pod reported 0/1 indefinitely with
//	      nothing timing it out: the exit code was in `kubectl logs` and nowhere a
//	      person looks first. provision.Spec.Health plus the kubernetes driver's
//	      startup/liveness pair is what reports it now.
//	      🔴 SO WHAT IS STILL OWED IS THE SAME SENTENCE, NARROWED RATHER THAN
//	      DISCHARGED: A REAL TURN THROUGH *MUSTER'S OWN GATEWAY* TO A
//	      *MUSTER-PROVISIONED* AGENT HAS STILL NOT HAPPENED. What has happened is a
//	      turn against the same IMAGE, configured by hand, from a host — which proves
//	      the wire contract and proves nothing about muster's wiring reaching it. Four
//	      things remain unproven in-repo, and each is unprovable here by construction:
//	      that the operator's install script does what it says (it lives in a
//	      ConfigMap this repository never sees); that the probe PASSES against a real
//	      kubelet; that the agent's Service name resolves; and that a turn through
//	      api.Extensions.Gateway authenticates. A fake clientset resolves no DNS and
//	      runs no pod.
//	      WHO CHECKS IT, AND HOW — this is the mechanical closing condition, not
//	      "tests pass". On a cluster, with MUSTER_AGENT_PROVISIONER=kubernetes, a
//	      runtime named, and the ConfigMap applied:
//
//	        kubectl -n <agent-ns> get deploy <agent> -o jsonpath='{.status.readyReplicas}'   # want 1
//	        kubectl -n <agent-ns> get pod -l app.kubernetes.io/instance=<agent> \
//	          -o jsonpath='{.items[0].status.containerStatuses[0].restartCount}{"\n"}'
//	        # the derived bearer, from the agent's own env secret. The object name is
//	        # k8s.envSecretName's — <agent>-env, not <agent>:
//	        T=$(kubectl -n <agent-ns> get secret <agent>-env \
//	          -o jsonpath='{.data.MUSTER_GATEWAY_BEARER}' | base64 -d)
//	        kubectl -n <agent-ns> run probe --rm -i --image=curlimages/curl --restart=Never -- \
//	          curl -sS -o /dev/null -w '%{http_code}\n' -X POST -H "Authorization: Bearer $T" \
//	          -H 'Content-Type: application/json' -d '{"model":"not-a-model","input":"x"}' \
//	          http://<agent>.<agent-ns>.svc:18789/v1/responses
//
//	      A 400 is the PASS — it means the route exists and the credential was
//	      accepted; the model field is what was refused. 🔴 A 404 IS A FAIL AND MEANS
//	      THE ROUTE IS NOT REGISTERED, which is a defect in the operator's TEMPLATE,
//	      not in the credential: re-run with a deliberately wrong bearer and confirm
//	      it ALSO 404s rather than 401ing, which is the only thing that separates the
//	      two. Then the same POST driven through muster's own chat tier, which is the
//	      turn this condition actually asks for.
//	      ⚠ THE OWNER OF THAT LAST MEASUREMENT IS CITED IN A SEPARATE SENTENCE ON
//	      PURPOSE, and the reason is a GUARD rather than style.
//	      TestTheRetractedReachabilityClaimIsGoneFromEveryNonTestSource sweeps this
//	      tree for the retracted claim as a RELATIONSHIP — the pair of words naming
//	      the two chat endpoints, within 80 characters of a reachability word — and
//	      the file that owns the on-cluster controls carries one of those
//	      reachability words in its own NAME. Citing it beside the phrase therefore
//	      tripped the sweep on a FILENAME. The guard was right about the shape and
//	      the sentence was not making the claim, so the citation moved instead of the
//	      allowlist growing an entry for a false positive — an allowlist entry is a
//	      licence, and licensing this shape would license the real claim with it. The
//	      owner is the Makefile's test-liveenv target, whose controls live under
//	      internal/agentgateway and name the variables they need.
//	  WHO CHECKED BOTH MECHANISMS: the reviewer of the pull request that declared the
//	      port and the second token name, against the two tests named above and
//	      against the mutation results in its own description. Neither blocker was a
//	      defect in the chat transport, and neither was introduced by the change that
//	      wired it.
//	  ✅ [WAS: "AND NOTHING ESCALATES THAT YET … NO loop drives it"] internal/agentkickoff
//	  drives agents.DecideReconcile for owed rows and TAKES its ActionError past
//	  ProvisioningStuckTimeout, so an undelivered kickoff now becomes `error` on its
//	  own — a red status dot, with the last send failure in error_message.
//	CLOSING CONDITION FOR THE REMAINDER: a pull request in which a dispatch with
//	  kickoff=true delivers its note through api.Extensions.Gateway — or the
//	  reconcile loop that escalates a missing one. ⚠ THE CALL SITE IS ONE OF THREE
//	  BLOCKERS, NOT THE ONLY ONE — an earlier revision of this line said "the
//	  transport and the wiring are no longer the blocker; the CALL SITE is", which
//	  named one and hid the two above it. A kickoff delivered through a gateway that
//	  can resolve no address, over a credential the container never received, is not
//	  a delivered kickoff.
//	  ⚠ IT IS THE LAST OF THE THREE NOW, AND THAT CHANGES WHAT A CALL SITE WOULD
//	  ACHIEVE WITHOUT CHANGING THE CONDITION. Both blockers above are closed on their
//	  mechanism, so a dispatch call site is no longer guaranteed to deliver nothing —
//	  but "no longer guaranteed to fail" is not "delivers", and the sentence above
//	  stays because it is the reason this condition was written as a DELIVERY rather
//	  than as a call. What a call site would now produce is the first real turn
//	  against an instance this binary created, which is also the evidence blocker (2)
//	  is still owed; whoever writes it gets both.
//	WHO CHECKS IT: the reviewer of that pull request, against agents.reconcile.go's
//	  header and against a dispatched agent's kickoff_error being empty.
//	✅ THE CALL SITE IS internal/agentkickoff; THE CONDITION'S OWN EVIDENCE IS STILL
//	  OWED. It asked for a DELIVERY, not a call, and a delivery is a fact about a
//	  cluster. What closes it now is mechanical and is the operator's: on a real cluster,
//	  with MUSTER_AGENT_GATEWAY=hooks-sha256 live, dispatch an agent with a kickoff
//	  and read its row once the pod is ready —
//	    SELECT kicked_off, kickoff_error, kickoff_attempts, kickoff_pod
//	      FROM agents WHERE name = '<agent>';
//	  — want kicked_off=t, kickoff_error='', kickoff_attempts=1, and an assistant
//	  reply to the pending note in that agent's chat transcript.
//
//	The original entry follows, kept rather than rewritten because its argument is
//	what the wrappers, the banner and the readiness defect are all still built on.
//
// ---------------------------------------------------------------------------
// 1b. 🔴 THE ORIGINAL ENTRY 1: WHY A FAKE WAS REFUSED WHILE THIS WAS NIL.
//
//	WHAT: api.Extensions.Provisioner is the agent-pod lifecycle driver — nine
//	  methods: Dispatch, Start, Stop, Destroy, Instances, TailLogs, StreamLogs,
//	  Chat and ChatWithTools. NOTHING in this module implements it. The only
//	  implementation of that interface anywhere in this tree is a stub in
//	  internal/api/registration_test.go, which exists to prove route
//	  registration does not branch on it.
//	WHY IT IS NIL RATHER THAN FAKED: the upstream implementation is
//	  internal/agents/provision.go, which deliberately STAYED with the
//	  permission router — it is a helm/client-go client to a specific cluster,
//	  not domain logic. internal/provision is muster's from-scratch replacement
//	  and it is a DIFFERENT SHAPE: a driver contract (Create/Update/Scale/…)
//	  over instances, with a noop driver and a Kubernetes driver. Adapting one to
//	  the other is a chunk of work with its own design question — where
//	  gateway chat and tool dispatch live, since provision.Driver has no notion
//	  of either. Wiring a fake here would make the Agents tab LOOK operational:
//	  dispatch would report success, no pod would exist, and the agent would sit
//	  in `provisioning` for ever. That is the "lies silently" side of the line.
//	WHAT IT COSTS MEANWHILE, PRECISELY: every agent-control route is REGISTERED
//	  — registration in internal/api is unconditional by design, which is what
//	  makes the route golden a claim about production — and each one refuses at
//	  request time with 503 + provisionerUnwired:true, through the
//	  api.requireLifecycleProvisioner / api.requireGatewayProvisioner wrapper it is
//	  registered behind. The Agents tab is
//	  read-only: existing rows render, nothing can be provisioned, started,
//	  stopped or destroyed, no logs stream and no chat turn runs. The boot banner
//	  says so on every start, unconditionally.
//	  🔴 THE ROUTE GOLDEN DOES NOT MOVE FOR THIS. If it ever does, the nil has
//	  started changing the route set, which is a defect in its own right.
//	  each is a WRAPPER for exactly that reason — the golden records
//	  patterns, not handlers, so the honest refusal costs no route drift.
//
//	🔴 THE SENTENCE ABOVE WAS FALSE WHEN IT WAS FIRST WRITTEN, AND THAT IS
//	  RECORDED HERE RATHER THAN QUIETLY CORRECTED. "Each one refuses at request
//	  time" was a claim about a wrapper that did not exist. Measured against the
//	  image this tree builds, with Provisioner nil, NINE routes answered 200 and
//	  did nothing — POST /agents, POST /agents/{id}/start, POST /agents/{id}/stop,
//	  DELETE /agents/{id}, GET /agents/{name}/logs/stream, GET /agents/{name}/ws,
//	  POST /agent/provision, POST /runbooks/{id}/dispatch and
//	  POST /api/agents/{name}/messages. A dispatch rendered a card over a row that
//	  stayed `provisioning`; a delete removed the card and left the row; the log
//	  stream wrote `: connected` and closed, which a reader takes for "no logs".
//	  🔴 THAT IS THE IDENTICAL OBSERVABLE THIS ENTRY REJECTS A FAKE FOR, six
//	  paragraphs up. Wiring nil and wiring a fake were the same lie; only the
//	  wrapper made the nil the honest option this entry always claimed it was.
//	  POST /agent/provision was worse than a lie: its goroutine was bare, so the
//	  nil deref killed the process rather than being recovered.
//	⚠ THE WRAPPER'S LEDGER IS THE THING TO UPDATE, NOT THIS PROSE. The routes
//	  above are pinned by TestEveryProvisionerRouteRefusesWhenItIsUnwired in
//	  internal/api/provisioner_seam_test.go, which fails when the set GROWS or
//	  SHRINKS. A tenth route reaching Provisioner without the wrapper reddens
//	  there, which is the mechanical half this paragraph cannot be.
//	CLOSING CONDITION (MET for the lifecycle half — see entry 1 above): a pull
//	  request adding an adapter from provision.Driver to api.Provisioner — in its
//	  own package, consumer-side interfaces unchanged — wired here behind the
//	  configuration that names a driver, with
//	  internal/provision/provisiontest's contract suite green against it. When
//	  it lands, internal/provision/k8s LEAVES the not-linked ledger in
//	  internal/modulegate/linkage_test.go, and that departure is the mechanical
//	  signal this seam closed.
//	WHO CHECKS IT: the reviewer of that pull request, against the provisiontest
//	  contract results and the linkage ledger.
//
// ---------------------------------------------------------------------------
// 2. ✅ REMOVED: api.PrivilegeApplier, AND THE WHOLE PRIVILEGE SUBSYSTEM.
//
//	This entry used to record the privilege applier seam (internal/agentprivilege
//	over the provisioning driver, behind MUSTER_AGENT_PRIVILEGE_APPLY) and the
//	readiness defect a wired Provisioner beside an unarmed applier raised. The
//	operator retired privileges (superseded by connectors): the store, routes,
//	views, tool, applier, the driver's RBAC grant path and the tables (migration
//	0005) are gone, so there is no seam left to close and no defect to raise.
//	MUSTER_AGENT_PRIVILEGE_APPLY is no longer read; like any unknown MUSTER_*
//	name it is ignored (config never refuses one).
//
// ---------------------------------------------------------------------------
// 3. ✅ api.ProfileReapplier IS SATISFIED, AND THE ASSERTION IS ASSERTED.
//
//	THE ANSWER THIS ENTRY DEMANDED, IN ONE LINE: *agentprovision.Adapter DOES
//	  implement ReapplyProfiles — it re-renders the agent's spec from its current
//	  row and calls the driver's Update, which reconciles the live instance and
//	  rolls it. TestTheLifecycleAdapterSatisfiesTheConsumerInterface performs the
//	  type assertion, and
//	  TestReapplyProfilesReconcilesRatherThanRecreates asserts the driver call
//	  rather than a nil return — because a body that returned nil and did nothing
//	  would satisfy the assertion, compile, wire, and make every model change a
//	  silent no-op.
//	⚠ ONE LATENT CONSEQUENCE, INTENDED: rolling an instance re-delivers its
//	  kickoff once a gateway exists, which agents.kickoffLost's own doc calls
//	  intended and agents.MaxKickoffAttempts caps. Nothing delivers a kickoff
//	  today, so it is dormant.
//
//	The original entry follows; its reasoning about type assertions is what the
//	  new test exists to answer, and it is worth keeping for the next optional
//	  dependency.
//
// ---------------------------------------------------------------------------
// 3b. api.ProfileReapplier WAS UNREACHABLE, WHICH IS ONE STEP BEYOND NIL.
//
//	WHAT: the interface re-renders a running agent's spec (it was written for the
//	  since-removed privilege grant path; the model change is its caller now). It
//	  has no field of its own: the caller TYPE-ASSERTS the Provisioner for it. With a nil Provisioner the assertion can never
//	  succeed, so this is not a dependency that is absent — it is a branch that
//	  cannot be taken.
//	WHY THAT IS WORTH WRITING DOWN SEPARATELY: because it will silently start
//	  working, or silently not, when entry 1 closes. An adapter that does not
//	  implement ReapplyProfiles compiles, wires, and takes the else branch for
//	  ever — with no nil anywhere for a reader to notice. A type assertion is the
//	  one form of optional dependency that leaves no trace when it is not
//	  satisfied.
//	CLOSING CONDITION: the pull request that closes entry 1 states, in its
//	  description, whether its adapter satisfies ProfileReapplier — and if it
//	  does, adds a test that the assertion succeeds. "It compiles" is not that
//	  test; the assertion is about a method set, and a method added to the
//	  wrong type compiles just as well.
//	WHO CHECKS IT: the reviewer of that pull request.
//
// ---------------------------------------------------------------------------
// 4. THE BACKGROUND LOOPS RUN UNGATED: THIS DEPLOYMENT IS SINGLE-REPLICA.
//
//	WHAT: api.Server.SetTaskLeaderGate takes a cross-process singleton gate for
//	  RunTaskRetention and RunTaskReap, and a nil gate means "unguarded". This
//	  binary passes nothing, because internal/db carries no lease — the upstream
//	  implementation is a Postgres session advisory lock that went with the
//	  permission router's half of the loop split.
//	WHY IT IS TOLERATED RATHER THAN FATAL: at replicas: 1 the loops are exactly
//	  correct, and both are idempotent row-level sweeps — a `DELETE … older
//	  than` and a tag-and-comment reap whose store takes `SELECT … FOR UPDATE`
//	  as its own command, which serialises across PROCESSES. So the cost of a
//	  doubled run is duplicated WORK, not duplicated EFFECT. It is not zero.
//	🔴 WHAT IT WOULD COST AT replicas > 1, STATED SO NOBODY SCALES THIS BY
//	  ACCIDENT: every prune and every reap runs once per pod, including across
//	  the overlap window of every rolling update. Deploy this single-replica
//	  until the lease exists.
//	CLOSING CONDITION: a pull request adding a lease to internal/db (a Postgres
//	  session advisory lock is the shape upstream uses) and passing it to
//	  SetTaskLeaderGate here, with a test that two processes cannot both hold
//	  it. Only then may the deployment carry more than one replica.
//	WHO CHECKS IT: the reviewer of that pull request, and whoever writes the
//	  first deployment manifest — the replica count is the thing to look at.
//
// ---------------------------------------------------------------------------
// 5. 🔴 A CLAUDE-CODE AGENT'S NetworkPolicy IS WRITTEN, NOT ENFORCED, BY THIS
//    BINARY — AND AN AGENT THAT PREDATES IT HAS NONE UNTIL IT IS RESTARTED.
//
//	WHAT: with the claude-code kind on the kubernetes driver, every claude-code
//	  agent's spec declares network isolation (agentspec.ClaudeCodeNetwork) and
//	  the driver writes a per-agent NetworkPolicy BEFORE the agent's Secret and
//	  Deployment, refusing the agent if it cannot (internal/provision/k8s
//	  network.go). Two things about that are NOT closed by any test here:
//	  (a) ENFORCEMENT. A NetworkPolicy is a declaration the cluster's network
//	      plugin acts on, or does not. Every test in this module runs against a
//	      fake clientset, which stores the object and confines nothing, so the
//	      suite is structurally blind to whether a packet is dropped — including
//	      whether the kubelet's own probes and muster's own turns still get
//	      through. The boot banner says WRITTEN for that reason.
//	  (b) AGENTS CREATED BEFORE THIS. Nothing at boot reconciles them. What
//	      reaches the driver's apply is a dispatch, a claude-code Start (always;
//	      a gateway-kind Start only when its instance is missing) and
//	      ReapplyProfiles. So a claude-code agent that was already running keeps
//	      running with NO policy until its next SUCCESSFUL apply — in practice,
//	      until an operator stops it and starts it.
//	  (c) THE SELECTOR IS NOT IN THE FINGERPRINT. MUSTER_AGENT_NETPOL_FROM_* is
//	      driver configuration, not part of an agent's spec, so changing it
//	      rewrites an existing agent's policy only at that agent's next apply.
//	      The boot banner prints the configured selector; it is what a NEW
//	      policy gets, not a reading of the policies that exist.
//	WHY (b) IS NOT A BOOT-TIME SWEEP: the driver's reconcile is Update, which
//	  sets one replica — a sweep would start every agent the operator had
//	  stopped — and it rolls the pod, because the network declaration is in the
//	  spec fingerprint the pod template carries. Writing the policy alone, without
//	  the rest of apply, would be a second write path for the one object whose
//	  ordering before the Deployment is the fail-closed guarantee.
//	WHAT A REFUSAL LOOKS LIKE: muster's ServiceAccount needs get, create, update
//	  and delete on networkpolicies.networking.k8s.io
//	  (k8s.NetworkPolicyRBACPrerequisite). Without it a claude-code dispatch or
//	  start fails, an agent that was not running is not started, and
//	  agents.error_message names the verb and the rule.
//	  🔴 TWO THINGS THAT REFUSAL DOES NOT DO. It does not STOP an agent that was
//	  already running: that agent is left exactly as it was, which for one that
//	  predates this build can mean running with no policy (the message says the
//	  agent may be in that state, and gives the command that checks). And WHERE
//	  that message lands depends on the path: a refused Dispatch or Start
//	  records it on the row (agentprovision.Adapter.fail, best-effort), while
//	  a refused ReapplyProfiles returns the failed Update's error to its caller
//	  without writing agents.error_message, and both of its callers in
//	  internal/api only LOG it. Those callers run it only for a row that reads
//	  running or provisioning — so a refusal on that path concerns an agent that
//	  may be up, and is in this server's log and not on the agent's row.
//	  🔴 GATEWAY-KIND AGENTS ARE UNAFFECTED ON CREATE, UPDATE AND SCALE — their
//	  specs declare no isolation, so the driver renders no policy and makes no
//	  networking call for them — AND NOT ON DESTROY. On this deployment the
//	  driver is configured for isolation, and its Destroy reads networkpolicies
//	  for EVERY instance (it has no spec to tell it which were isolated). With
//	  the rule missing, destroying a gateway agent removes its Deployment and
//	  then fails, keeping the row, until the rule exists. APPLY THE RBAC RULE
//	  BEFORE DEPLOYING A BUILD WITH THIS KIND ENABLED.
//	CLOSING CONDITION for (a): on a real cluster, from inside a claude-code
//	  agent's pod, the cluster API, another namespace's Service and a private
//	  address each fail to connect while a public HTTPS endpoint answers — the
//	  PAIR, not the failures alone — AND the pod stays Ready and answers a turn
//	  from muster. For (b): every claude-code agent that predates this build has
//	  been stopped and started (or destroyed), checked by
//	  `kubectl get networkpolicy -A -l app.kubernetes.io/managed-by=muster`
//	  listing one policy per claude-code agent namespace. (c) has no closing
//	  condition: it is how the configuration works, recorded so a selector
//	  change is followed by restarting the agents it should apply to.
//	WHO CHECKS IT: the operator deploying this build, on their cluster.
//
// ---------------------------------------------------------------------------
// 6. NOT A SEAM: WHAT IS NIL HERE ON PURPOSE AND CLOSES NOTHING.
//
//   - api.Extensions.GitHub is nil without MUSTER_GITHUB_ENCRYPTION_KEY. There
//     is deliberately no generate-a-key-if-unset branch: an ephemeral key makes
//     every stored token undecryptable after a restart, which presents as "the
//     Repos tab forgot my account" with nothing in the log. Unset means the
//     store is not built, which the Repos tab says on screen.
//   - api.RouterPort and api.GatePort are nil without a router. That IS a
//     supported deployment — see RouterPort's own doc — and the four features
//     that depend on it each degrade where a human can see it. The one bit that
//     must NOT be inferred is session liveness, which is why MUSTER_STANDALONE
//     exists and why /readyz refuses without it. See buildSessionLiveness.
//
// ---------------------------------------------------------------------------
