package commands

import (
	"bytes"
	"errors"
	"github.com/Shopify/themekit"
	"net/http"
	"os"
)

const (
	MasterBranch   = "master"
	LatestRelease  = "latest"
	ThemeZipRoot   = "https://github.com/Shopify/Timber/archive/"
	TimberFeedPath = "https://github.com/Shopify/Timber/releases.atom"
)

type BootstrapOptions struct {
	BasicOptions
	Version     string
	Directory   string
	Environment string
	Prefix      string
	SetThemeId  bool
}

func BootstrapCommand(args map[string]interface{}) chan bool {
	options := BootstrapOptions{}

	extractString(&options.Version, "version", args)
	extractString(&options.Directory, "directory", args)
	extractString(&options.Environment, "environment", args)
	extractString(&options.Prefix, "prefix", args)
	extractBool(&options.SetThemeId, "setThemeId", args)
	extractThemeClient(&options.Client, args)
	extractEventLog(&options.EventLog, args)

	return Bootstrap(options)
}

func Bootstrap(options BootstrapOptions) chan bool {
	done := make(chan bool)
	go func() {
		doneCh := doBootstrap(options)
		done <- <-doneCh
	}()
	return done
}

func doBootstrap(options BootstrapOptions) chan bool {
	pwd, _ := os.Getwd()
	if pwd != options.Directory {
		os.Chdir(options.Directory)
	}

	zipLocation, err := zipPathForVersion(options.Version)
	if err != nil {
		themekit.NotifyError(err)
		done := make(chan bool)
		close(done)
		return done
	}

	name := "Timber-" + options.Version
	if len(options.Prefix) > 0 {
		name = options.Prefix + "-" + name
	}
	clientForNewTheme, themeEvents := options.Client.CreateTheme(name, zipLocation)
	mergeEvents(options.getEventLog(), []chan themekit.ThemeEvent{themeEvents})
	if options.SetThemeId {
		AddConfiguration(options.Directory, options.Environment, clientForNewTheme.GetConfiguration())
	}

	os.Chdir(pwd)

	downloadOptions := DownloadOptions{}
	downloadOptions.Client = clientForNewTheme
	downloadOptions.EventLog = options.getEventLog()

	done := Download(downloadOptions)

	return done
}

func zipPath(version string) string {
	return ThemeZipRoot + version + ".zip"
}

func zipPathForVersion(version string) (string, error) {
	if version == MasterBranch {
		return zipPath(MasterBranch), nil
	}

	feed, err := downloadAtomFeed()
	if err != nil {
		return "", err
	}

	entry, err := findReleaseWith(feed, version)
	if err != nil {
		return "", err
	}

	return zipPath(entry.Title), nil
}

func downloadAtomFeed() (themekit.Feed, error) {
	resp, err := http.Get(TimberFeedPath)
	if err != nil {
		return themekit.Feed{}, err
	}
	defer resp.Body.Close()

	feed, err := themekit.LoadFeed(resp.Body)
	if err != nil {
		return themekit.Feed{}, err
	}
	return feed, nil
}

func findReleaseWith(feed themekit.Feed, version string) (themekit.Entry, error) {
	if version == LatestRelease {
		return feed.LatestEntry(), nil
	}
	for _, entry := range feed.Entries {
		if entry.Title == version {
			return entry, nil
		}
	}
	return themekit.Entry{Title: "Invalid Feed"}, buildInvalidVersionError(feed, version)
}

func buildInvalidVersionError(feed themekit.Feed, version string) error {
	buff := bytes.NewBuffer([]byte{})
	buff.Write([]byte(themekit.RedText("Invalid Timber Version: " + version)))
	buff.Write([]byte("\nAvailable Versions Are:"))
	buff.Write([]byte("\n  - master"))
	buff.Write([]byte("\n  - latest"))
	for _, entry := range feed.Entries {
		buff.Write([]byte("\n  - " + entry.Title))
	}
	return errors.New(buff.String())
}
