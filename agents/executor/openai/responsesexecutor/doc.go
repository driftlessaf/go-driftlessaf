/*
Copyright 2026 Chainguard, Inc.
SPDX-License-Identifier: Apache-2.0
*/

// Package responsesexecutor runs stateless, streaming Responses conversations
// with client-side tools and validated terminal submissions. Clients and route
// attribution are supplied by the caller; this package never loads credentials.
//
// # Suspend and Resume
//
// Config.SuspendTool advertises a held-out tool that pauses the run. When the
// model calls it, Execute waits for the turn's other tool calls to finish,
// then returns a *checkpoint.Suspension whose ProviderState holds every input
// item the next request would replay, encrypted reasoning included, byte for
// byte. The executor New returns also implements Resumer and ResultResumer:
// Resume and ResumeWithResults check the envelope against the live
// configuration (checkpoint.ErrConfigDrift on a mismatch), send the paused
// turn's function outputs with the answer, and continue the conversation.
// Config.SuspendOnSubmit pauses on every accepted submission instead of
// ending the run.
package responsesexecutor
