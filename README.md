# gitlab-fingerprinter

A GitLab version fingerprinting tool written in Go that:
- retrieves the *exact* version and edition of a GitLab environment;
- returns if the version is outdated or end-of-life (EOL);
- can scan multiple environments at once;
- outputs the results in an easy to process JSON format;

E.g.:
```
{
  "results": [
    {
      "target": "gitlab.example.foo",
      "version": "16.8.7",
      "edition": "community",
      "end_of_life": true,
      "outdated": true,
      "warnings": [
        "16.8.x is end-of-life (EOL), see https://endoflife.date/gitlab"
      ]
    },
    {
      "target": "git.example.bar",
      "version": "16.8.5",
      "edition": "enterprise",
      "end_of_life": true,
      "outdated": true,
      "warnings": [
        "16.8.x is end-of-life (EOL), see https://endoflife.date/gitlab",
        "16.8.5 is outdated, latest 16.8 version is 16.8.7"
      ]
    }
  ],
  "errors": null
}
```


## Usage

```
git clone https://github.com/jersec/gitlab-fingerprinter
go run . gitlab.example.com git.example.foo
```

## How does it work?

### The webpack manifest hash

GitLab uses webpack and each GitLab environment has a public webpack Manifest file located at `/assets/webpack/manifest.json` in which a hash, unique to each webpack build, is present.

If you create a dictionary of hashes for each GitLab build (version), you can fingerprint the GitLab version based on this hash by simply comparing the one in the Manifest file with the one in the created list of hashes. [@righel](https://github.com/righel/else) has done just this and runs a nightly GitHub action that retrieves these hashes and stores them into a file, which is then used by their Nmap script (NSE). *gitlab-fingerprinter makes* use of [this list](https://raw.githubusercontent.com/righel/gitlab-version-nse/main/gitlab_hashes.json) as well.

*But*, there is a problem with this tactic:  
If GitLab does not change front-end code, the Manifest hash does not change. This results in multiple patch versions being returned. For example, 16.8.6 and 16.8.7 share the same hash.

But the purpose of this tool is to fingerprint the *exact* version, so how can we do this?

### The creation date

I found that the Manifest file, when retrieved, contains the "Last-Modified" header and that the date in it is the actual datetime at which the file was built.

We can use this information as follows:
- From the list of returned versions we retrieve the *minor version* (*16.8 for example*).
- We make an API request to the GitLab project API and retrieve all tags for this minor version.
- We then use the retrieved *Last-Modified* date and compare it with the creation dates of the retrieved tags:
    - We known that the file is built *after* the tag is created due to the GitLab build process flow, so we are looking for tags that are created *before* the *Last-Modified* date of the Manifest file.
    - We then calculate which tag was created closest to the *Last-Modified* date.

Using this logic we can guess the exact version number **99.9999% certainty**.  
(*The remaining 0.0001% is reserved for when the GitLab team decides to tag two different patch versions for the same minor versions within a couple of minutes, which is highly unlikely.*)

### The report

The rest is straight-forward: we use the [https://endoflife.date/](https://endoflife.date/) public API to retrieve the status of all GitLab versions and check if the fingerprinted version is either outdated and/or end-of-life (EOL).


***Enjoy!***
