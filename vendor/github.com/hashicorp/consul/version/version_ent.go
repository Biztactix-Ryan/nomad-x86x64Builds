// +build consulent

package version

func init() {
	// The main version number that is being run at the moment.
	Version = "1.7.3+ent"

	// A pre-release marker for the version. If this is "" (empty string)
	// then it means that it is a final release. Otherwise, this is a pre-release
	// such as "dev" (in development), "beta", "rc1", etc.
	VersionPrerelease = ""
}
