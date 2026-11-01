// Package guestassets contains first-party one-shot guest hooks, not a service.
package guestassets

import _ "embed"

//go:embed repository_profile.py
var RepositoryProfile string

//go:embed repository_exec.py
var RepositoryExec string
