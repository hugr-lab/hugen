// Package manager owns the multi-session supervisor that opens,
// resumes, and terminates root sessions plus the boot-time
// recovery walker that settles dangling sub-agents after a process
// restart. The Session itself (turn loop, frame routing, tool
// dispatch, persistence projection) lives in pkg/session.
//
// This file is a Stage C-25 stub for phase 4.1b-pre. Commit 26
// moves Manager from pkg/session/manager.go → here.
package manager
