package paths

import "os"

// Env provides logging overrides and the runtime home lookup.
type Env interface {
	Get(string) string
	Home() (string, error)
}
type OSEnv struct{}

func (OSEnv) Get(name string) string { return os.Getenv(name) }

func (OSEnv) Home() (string, error) { return os.UserHomeDir() }
