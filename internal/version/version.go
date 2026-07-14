// Package version holds the single source of truth for the weightsweep
// version string. It is embedded in --version output and in generated
// prune plans so a plan records which tool build produced it.
package version

// Version is the semantic version of this build.
const Version = "0.1.0"
