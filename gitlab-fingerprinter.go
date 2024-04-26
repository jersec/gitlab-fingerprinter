// A GitLab Fingerprinter tool written in Go by Jeroen Swen
// Licensed under the GNU General Public License v3.0
// https://github.com/jersec/gitlab-fingerprinter

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const endOfLifeDateApiURL = "https://endoflife.date/api/gitlab.json"
const hashesURL = "https://raw.githubusercontent.com/righel/gitlab-version-nse/main/gitlab_hashes.json"
const tagsApiURL = "https://gitlab.com/api/v4/projects/278964/repository/tags"

type HashDictionary map[string]struct {
	Build    string   `json:"build"`
	Versions []string `json:"versions"`
}

type GitlabTag struct {
	Name          string    `json:"name"`
	CreatedAtDate time.Time `json:"created_at"`
}
type GitlabTags []GitlabTag

type GitlabVersion struct {
	Cycle             string `json:"cycle"`
	EOL               string `json:"eol"`
	Latest            string `json:"latest"`
	LatestReleaseDate string `json:"latestReleaseDate"`
	ReleaseDate       string `json:"releaseDate"`
}
type GitlabVersions []GitlabVersion

type Manifest struct {
	Hash             string `json:"hash"`
	LastModifiedDate time.Time
	OutputPath       string `json:"outputPath"`
}

type Result struct {
	Target    string   `json:"target"`
	Version   string   `json:"version"`
	Edition   string   `json:"edition"`
	EndOfLife bool     `json:"end_of_life"`
	Outdated  bool     `json:"outdated"`
	Warnings  []string `json:"warnings"`
}

type Error struct {
	Target  string `json:"target"`
	Error   string `json:"error"`
	Details string `json:"details"`
}

type FinalOutput struct {
	Results []Result `json:"results"`
	Errors  []Error  `json:"errors"`
}

// Cache GitLab API results per minor version.
var gitlabTagsCache = make(map[string]GitlabTags)

func main() {
	if len(os.Args) < 2 {
		fmt.Println("usage: gitlab-fingerprint <url>")
		return
	}

	// Display help info on certain arguments.
	for _, arg := range os.Args {
		if arg == "-h" || arg == "--help" || arg == "-v" || arg == "--version" {
			fmt.Println("A GitLab Fingerprinting tool by Jeroen Swen (https://github.com/jersec/gitlab-fingerprinter)")
			fmt.Println("usage: gitlab-fingerprint <url> <url> <url>")
			fmt.Println("Examples:")
			fmt.Println("gitlab-fingerprinter https://gitlab.foo.com")
			fmt.Println("gitlab-fingerprinter https://gitlab.example.com gitlab.example.foo http://git.example.bar")
			return
		}
	}

	var FinalOutput FinalOutput

	// Process URLs.
	var targetURLs []*url.URL
	for i, arg := range os.Args {
		// Skip the first argument.
		if i == 0 {
			continue
		}

		// If no scheme exists, we add one.
		if !strings.HasPrefix(arg, "http://") && !strings.HasPrefix(arg, "https://") {
			arg = "https://" + arg
		}

		// Parse the URL.
		targetURL, err := url.ParseRequestURI(arg)
		if err != nil {
			var newError Error
			newError.Target = targetURL.Host
			newError.Error = fmt.Sprintf("The URL '%s' is not valid", arg)
			newError.Details = err.Error()
			FinalOutput.Errors = append(FinalOutput.Errors, newError)
			continue
		}

		// Verify if the domain resolves to an IP address.
		ips, err := net.LookupIP(targetURL.Hostname())
		if err != nil || len(ips) == 0 {
			var newError Error
			newError.Target = targetURL.Host
			newError.Error = fmt.Sprintf("Could not resolve '%s'", targetURL)
			newError.Details = err.Error()
			FinalOutput.Errors = append(FinalOutput.Errors, newError)
			continue
		}

		// Set path to the public GitLab webpack manifest file.
		targetURL.Path = "/assets/webpack/manifest.json"

		// Add to target list.
		targetURLs = append(targetURLs, targetURL)
	}

	// Retrieve GitLab hash dictionary from https://github.com/righel/gitlab-version-nse.
	hashDictionary, err := getHashDictionary()
	if err != nil {
		err = fmt.Errorf("failed to retrieve GitLab hash dictionary from https://github.com/righel/gitlab-version-nse: %v", err)
		log.Fatal(err)
	}

	// Retrieve GitLab versions info from endoflife.date API.
	gitlabVersionsInfo, err := getGitlabVersionsInfo()
	if err != nil {
		err = fmt.Errorf("error retrieving GitLab product information from endoflife.date API: %v", err)
		log.Fatal(err)
	}

	// Iterate through all targets.
	for _, targetURL := range targetURLs {
		manifest, err := getManifest(targetURL.String())
		if err != nil {
			var newError Error
			newError.Target = targetURL.Host
			newError.Error = "Failed to fingerprint target"
			newError.Details = err.Error()
			FinalOutput.Errors = append(FinalOutput.Errors, newError)
			continue
		}

		// If there is no mention of gitlab in the outputPath, the Manifest does not belong to a GitLab installation.
		if !strings.Contains(manifest.OutputPath, "gitlab") {
			var newError Error
			newError.Target = targetURL.Host
			newError.Error = "Target is not a GitLab installation"
			err = fmt.Errorf("the outputPath in %s has no mention of 'gitlab' in it", targetURL)
			newError.Details = err.Error()
			FinalOutput.Errors = append(FinalOutput.Errors, newError)
			err = nil
			continue
		}

		// Prepare a target result.
		var result Result

		result.Target = targetURL.Host

		// Iterate over hashes.
		var hashFound bool
		for dictionaryHash, info := range hashDictionary {
			if dictionaryHash == manifest.Hash {
				hashFound = true
				switch info.Build {
				case "gitlab-ee":
					result.Edition = "enterprise"
				case "gitlab-ce":
					result.Edition = "community"
				default:
					result.Edition = "unknown"
					var newError Error
					newError.Target = targetURL.Host
					newError.Error = "Could not determine Edition"
					newError.Details = fmt.Sprintf("the following edition was returned in the hash results: %s", info.Build)
					FinalOutput.Errors = append(FinalOutput.Errors, newError)
				}

				// If more than one version is returned we will try to guess the versions further.
				if len(info.Versions) == 1 {
					for _, version := range info.Versions {
						result.Version = version
					}
				} else {
					// Find the Tag where the creation date is before the Manifest Last-Modified date and closest to it.
					var closestDate time.Time
					var closestDateDifference time.Duration
					var closestDateTag string

					// Check if multiple minor versions are returned. Chance of this happening is neglectible, but handle this situation regardless.
					minorVersionsMap := make(map[string]bool)
					var resultMinorVersion string

					for _, version := range info.Versions {
						versionParts := strings.Split(version, ".")
						parsedMinorVersion := strings.Join(versionParts[:2], ".")
						minorVersionsMap[parsedMinorVersion] = true
						resultMinorVersion = parsedMinorVersion
					}

					if len(minorVersionsMap) > 1 {
						var newError Error
						newError.Target = targetURL.Host
						newError.Error = "Could not determine exact version"
						newError.Details = fmt.Sprintf("multiple minor versions were returned: %s", info.Versions)
						FinalOutput.Errors = append(FinalOutput.Errors, newError)
					} else {
						tags, err := getTagsForMinorVersion(resultMinorVersion)
						if err != nil {
							log.Fatal(err)
						}

						for _, tag := range tags {
							if tag.CreatedAtDate.Before(manifest.LastModifiedDate) {
								difference := manifest.LastModifiedDate.Sub(tag.CreatedAtDate)

								if closestDate.IsZero() || difference < closestDateDifference {
									closestDate = tag.CreatedAtDate
									closestDateDifference = difference
									closestDateTag = tag.Name
								}
							}
						}

						result.Version = strings.Replace(strings.Replace(closestDateTag, "v", "", -1), "-ee", "", -1)
					}
				}
				warnings := []string{}

				// Check if version is Outdated or EOL by using the endoflife.date data.
				resultVersionParts := strings.Split(result.Version, ".")
				resultMinorVersion := strings.Join(resultVersionParts[:2], ".")
				for _, gitlabVersion := range gitlabVersionsInfo {
					if gitlabVersion.Cycle == resultMinorVersion {
						eolDate, err := time.Parse("2006-01-02", gitlabVersion.EOL)
						if err != nil {
							fmt.Println("Error parsing date:", err)
							os.Exit(1)
						}

						currentDate := time.Now().Format("2006-01-02")
						parsedDate, err := time.Parse("2006-01-02", currentDate)
						if err != nil {
							fmt.Println("Error parsing date:", err)
							os.Exit(1)
						}

						if eolDate.Before(parsedDate) {
							warnings = append(warnings, fmt.Sprintf("%s.x is end-of-life (EOL), see https://endoflife.date/gitlab", resultMinorVersion))
							result.EndOfLife = true
							result.Outdated = true
						}

						if result.Version != gitlabVersion.Latest {
							warnings = append(warnings, fmt.Sprintf("%s is outdated, latest %s version is %s", result.Version, gitlabVersion.Cycle, gitlabVersion.Latest))
							result.Outdated = true
						}
					}
				}

				result.Warnings = warnings
			}
		}

		// If a hash was returned, but not found in the dictionary it can mean two things:
		if !hashFound {
			// The hash dictionary has not been updated yet, in this case we check if the Last-Modified date is less than 24 old.
			if manifest.LastModifiedDate.After(time.Now().Add(-24 * time.Hour)) {
				var result Result
				result.Target = targetURL.Host
				result.Version = "unknown"
				result.Edition = "unknown"
				result.EndOfLife = false
				result.Outdated = false
				result.Warnings = append(result.Warnings, "Could not fingerprint the version as the hash was not found in '%s'. However, "+
					"the installed version seems to be less than 24 hours old and is likely not indexed yet (which happens once a day). "+
					"It's therefore safe to assume that it's running a version released in the last 24 hours.", hashesURL)
				FinalOutput.Results = append(FinalOutput.Results, result)
			} else {
				// If longer than 24 hours old, the hash dictionary is no longer being updated.
				var newError Error
				newError.Target = targetURL.Host
				newError.Error = "Unable to guess version of target"
				newError.Details = fmt.Sprintf("A manifest file was found, but the hash in it (%s) was not found in '%s'. The Last-Modified "+
					"date of the manifest file (%s) is not shorter than 24 hours. The most likely culprit for this error is that the Hashes file is no "+
					"longer being updated. See: https://github.com/righel/gitlab-version-nse/",
					manifest.Hash, hashesURL, manifest.LastModifiedDate)
				FinalOutput.Errors = append(FinalOutput.Errors, newError)
			}
		}

		FinalOutput.Results = append(FinalOutput.Results, result)
	}

	jsonOutput, err := json.MarshalIndent(FinalOutput, "", "  ")
	if err != nil {
		err = fmt.Errorf(fmt.Sprintf("Failed to marshal output: %v", err), err)
		log.Fatal(err)
	}
	fmt.Println(string(jsonOutput))

}

func getGitlabVersionsInfo() (gitlabVersions GitlabVersions, err error) {
	resp, err := http.Get(endOfLifeDateApiURL)
	if err != nil {
		return
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		err = fmt.Errorf("%s did not respond with a 200 OK", endOfLifeDateApiURL)
		return
	}

	rawJSON, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if !json.Valid(rawJSON) {
		err = fmt.Errorf("%s did not return valid json", endOfLifeDateApiURL)
	}

	err = json.Unmarshal(rawJSON, &gitlabVersions)
	if err != nil {
		return
	}

	return
}

func getHashDictionary() (hashDictionary HashDictionary, err error) {
	resp, err := http.Get(hashesURL)
	if err != nil {
		return
	}

	defer resp.Body.Close()

	rawJSON, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if !json.Valid(rawJSON) {
		err = fmt.Errorf("%s did not return valid json", hashesURL)
	}

	err = json.Unmarshal(rawJSON, &hashDictionary)
	if err != nil {
		return nil, err
	}

	return
}

func getTagsForMinorVersion(minorVersion string) (gitlabTags GitlabTags, err error) {
	// Check if the tags for the given minor version are already in the cache.
	if cachedTags, ok := gitlabTagsCache[minorVersion]; ok {
		return cachedTags, nil
	}

	url := tagsApiURL + "?per_page=50&search=v" + minorVersion + ".*-ee"

	resp, err := http.Get(url)
	if err != nil {
		return
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		err = fmt.Errorf("%s did not respond with a 200 OK", url)
		return
	}

	if resp.Header.Get("content-type") != "application/json" {
		err = fmt.Errorf("%s did not respond with JSON", url)
		return
	}

	rawJSON, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if !json.Valid(rawJSON) {
		err = fmt.Errorf("%s did not return valid JSON file", url)
		return
	}

	err = json.Unmarshal(rawJSON, &gitlabTags)
	if err != nil {
		err = fmt.Errorf("%s did not return valid Manifest file: %v", url, err)
		return
	}

	for tag := range gitlabTags {
		tag := &gitlabTags[tag]

		// Parse the Created At date to the correct format:
		tag.CreatedAtDate, err = time.Parse("2006-01-02 15:04:05 -0700 MST", tag.CreatedAtDate.String())
		if err != nil {
			return
		}
	}

	// Store the tags in the cache.
	gitlabTagsCache[minorVersion] = gitlabTags

	return
}

func getManifest(url string) (manifest Manifest, err error) {
	resp, err := http.Get(url)
	if err != nil {
		return
	}

	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		err = fmt.Errorf("likely not a GitLab installation as %s did not respond with a 200 OK", url)
		return
	}

	rawJSON, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}

	if !json.Valid(rawJSON) {
		err = fmt.Errorf("likely not a GitLab installation as %s did not return valid json", url)
		return
	}

	err = json.Unmarshal(rawJSON, &manifest)
	if err != nil {
		err = fmt.Errorf("likely not a GitLab installation as %s did not return a (GitLab) webpack Manifest", url)
		return
	}

	lastModifiedTime, err := time.Parse("Mon, 02 Jan 2006 15:04:05 MST", resp.Header.Get("Last-Modified"))
	if err != nil {
		return
	}

	manifest.LastModifiedDate = lastModifiedTime

	return
}
