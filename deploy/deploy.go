// Package deploy holds the files that install aimd as a login service,
// embedded so the aim binary carries them: a launchd agent on a Mac, a
// systemd user unit on Linux.
package deploy

import _ "embed"

// LaunchdLabel is the launchd label of the aimd agent, and the plist's
// file name without .plist.
const LaunchdLabel = "com.gravexa.aimd"

// LaunchdEnv is the environment variable `aim up` sets to "1" in the
// plist's EnvironmentVariables, and in the systemd unit's Environment. It
// tells aimd a service manager runs it and keeps its stderr (launchd in
// aimd.launchd.log, systemd in the journal), so aimd logs only to aimd.log,
// and stderr keeps what aimd.log doesn't get: a panic, or the error aimd
// stops with. The name is from when launchd was the only one.
const LaunchdEnv = "AIMD_LAUNCHD"

// PlistTemplate is com.gravexa.aimd.plist, a text/template that `aim up`
// fills in. It expects an "xml" function that escapes text for XML, and
// fields Label, Program, Home, LaunchdLog and Env (a list with Key and
// Value).
//
//go:embed com.gravexa.aimd.plist
var PlistTemplate string

// SystemdUnit is the name of aimd's systemd user unit, and of its file.
const SystemdUnit = "aimd.service"

// UnitTemplate is aimd.service, a text/template that `aim up` fills in on
// Linux. It expects "exec", "dir" and "env" functions that write a
// command's program, a directory and a KEY=value pair the way systemd
// reads them, and fields Program, Home and Env (a list with Key and
// Value).
//
//go:embed aimd.service
var UnitTemplate string
