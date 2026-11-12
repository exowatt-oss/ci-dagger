package main

import "regexp"

// CiDagger is a collection of reusable Dagger-based CI primitives.
type CiDagger struct{}

// repoRE validates short repository identifiers used in cache names and
// optional database names. Registry prefixes remain caller-controlled.
var repoRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
