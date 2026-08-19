// Package worker is the data plane: lease, stream, report, repeat.
//
// A worker holds no database credentials and contains no SQL. It learns what
// to move from the Coordinator's worker plane (docs/design/09 §7) and moves it
// directly between registries - bytes never pass through the Coordinator and
// never touch this process's disk.
package worker
