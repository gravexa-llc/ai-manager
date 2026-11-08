// Package deploy holds the files that install ai-manager on a Mac, embedded
// so the aim binary carries them.
package deploy

import _ "embed"

// LaunchdLabel is the launchd label of the aimd agent, and the plist's
// file name without .plist.
const LaunchdLabel = "com.gravexa.aimd"

// LaunchdEnv is the environment variable `aim up` sets to "1" in the
// plist's EnvironmentVariables. It tells aimd it runs under launchd, whose
// stderr is aimd.launchd.log, so aimd logs only to aimd.log, and stderr
// keeps what aimd.log doesn't get: a panic, or the error aimd stops with.
const LaunchdEnv = "AIMD_LAUNCHD"

// PlistTemplate is com.gravexa.aimd.plist, a text/template that `aim up`
// fills in. It expects an "xml" function that escapes text for XML, and
// fields Label, Program, Home, LaunchdLog and Env (a list with Key and
// Value).
//
//go:embed com.gravexa.aimd.plist
var PlistTemplate string
