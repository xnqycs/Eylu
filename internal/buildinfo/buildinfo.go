package buildinfo

import "fmt"

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
	BuiltBy = "go build"
)

type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Date    string `json:"date"`
	BuiltBy string `json:"built_by"`
}

func Current() Info {
	return Info{Version: Version, Commit: Commit, Date: Date, BuiltBy: BuiltBy}
}

func String() string {
	return fmt.Sprintf("%s commit=%s date=%s built_by=%s", Version, Commit, Date, BuiltBy)
}
