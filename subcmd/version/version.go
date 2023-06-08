// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package version provides version subcommand.
package version

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/maruel/subcommands"
	"go.chromium.org/luci/cipd/version"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/hardcoded/chromeinfra"
)

func Cmd(ver string) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "version",
		ShortDesc: "prints the executable version",
		LongDesc:  "Prints the executable version and the CIPD package the executable was installed from (if it was installed via CIPD).",
		CommandRun: func() subcommands.CommandRun {
			r := &versionRun{version: ver}
			r.init()
			return r
		},
	}
}

type versionRun struct {
	subcommands.CommandRunBase
	version string
	cipdURL string
}

func (c *versionRun) init() {
	c.Flags.StringVar(&c.cipdURL, "cipd_url", "", "show version info for this cipd URL.")
}

func (c *versionRun) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	if len(args) != 0 {
		fmt.Fprintf(a.GetErr(), "%s: position arguments not expected\n", a.GetName())
		return 1
	}
	fmt.Println(c.version)
	cipdURL := c.cipdURL
	if cipdURL == "" {
		switch ver, err := version.GetStartupVersion(); {
		case err != nil:
			// Note: this is some sort of catastrophic error. If the binary is not
			// installed via CIPD, err == nil && ver.InstanceID == "".
			fmt.Fprintf(os.Stderr, "cannot determine CIPD package version: %s\n", err)
			return 1
		case ver.InstanceID == "":
			buildInfo, ok := debug.ReadBuildInfo()
			if !ok {
				return 0
			}
			if buildInfo.GoVersion != "" {
				fmt.Printf("go\t%s\n", buildInfo.GoVersion)
			}
			for _, s := range buildInfo.Settings {
				if strings.HasPrefix(s.Key, "vcs.") {
					fmt.Printf("build\t%s=%s\n", s.Key, s.Value)
				}
			}
			return 0
		default:
			fmt.Println()
			fmt.Printf("CIPD package name: %s\n", ver.PackageName)
			fmt.Printf("CIPD instance ID:  %s\n", ver.InstanceID)
			cipdURL = fmt.Sprintf("%s/p/%s/+/%s", chromeinfra.CIPDServiceURL, ver.PackageName, ver.InstanceID)
		}
	}
	fmt.Printf("CIPD URL: %s\n", cipdURL)

	// TODO(crbug.com//1451715): cleanup once cipd tag can easily identify the revision of the binary.
	repo, rev, err := parseCIPDGitRepoRevision(ctx, cipdURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get git_repository and git_revision in %s: %v\n", cipdURL, err)
		return 1
	}
	fmt.Printf("%s/+/%s\n", repo, rev)
	switch repo {
	case "https://chromium.googlesource.com/infra/infra_superproject":
		rev, err = parseInfraSuperprojectDEPS(ctx, rev)
		if err != nil {
			fmt.Fprintf(os.Stderr, "failed to get infra revision in infra_superproject.git@%s/DEPS: %v\n", rev, err)
			return 1
		}
		fmt.Printf("https://chromium.googlesource.com/infra/infra/+/%s\n", rev)
	case "https://chromium.googlesource.com/infra/infra":
	default:
		fmt.Fprintf(os.Stderr, "unknown git_repository: %s\n", repo)
		return 1
	}
	sisoCommit, err := getSisoCommit(ctx, rev)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to get siso commit in infra.git@%s: %v\n", rev, err)
		return 1
	}
	fmt.Println(sisoCommit)
	return 0
}

func parseCIPDGitRepoRevision(ctx context.Context, cipdURL string) (repo, rev string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cipdURL, nil)
	if err != nil {
		return "", "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return "", "", fmt.Errorf("http=%d %s", resp.StatusCode, resp.Status)
	}
	s := bufio.NewScanner(resp.Body)
	var repository, revision string
	for s.Scan() {
		line := s.Bytes()
		if bytes.Contains(line, []byte("git_repository:")) {
			repository = string(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("git_repository:")))
		}
		if bytes.Contains(line, []byte("git_revision:")) {
			revision = string(bytes.TrimPrefix(bytes.TrimSpace(line), []byte("git_revision:")))
		}
	}
	err = s.Err()
	if err != nil {
		return "", "", err
	}
	if repository == "" || revision == "" {
		return "", "", fmt.Errorf("git_repository, git_revision not found in %s", cipdURL)
	}
	return repository, revision, nil
}

func parseInfraSuperprojectDEPS(ctx context.Context, rev string) (string, error) {
	depsURL := fmt.Sprintf("https://chromium.googlesource.com/infra/infra_superproject/+/%s/DEPS?format=TEXT", rev)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, depsURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return "", fmt.Errorf("http=%d %s", resp.StatusCode, resp.Status)
	}
	s := bufio.NewScanner(base64.NewDecoder(base64.StdEncoding, resp.Body))
	var revision string
	for s.Scan() {
		line := s.Bytes()
		if !bytes.Contains(line, []byte("infra/infra.git@")) {
			continue
		}
		i := bytes.IndexByte(line, '@')
		if i < 0 {
			continue
		}
		revision = string(line[i+1:])
		i = strings.IndexByte(revision, '"')
		if i >= 0 {
			revision = revision[:i]
		}
	}
	err = s.Err()
	if err != nil {
		return "", err
	}
	if revision == "" {
		return "", fmt.Errorf("infra/infra.git not found in %s", depsURL)
	}
	return revision, nil
}

type commit struct {
	revision string
	summary  string
	author   string
	date     time.Time
}

func (c commit) String() string {
	return fmt.Sprintf("%s %s\n %s by %s", c.revision[:10], c.summary, c.date.Format(time.RFC3339), c.author)

}

func getSisoCommit(ctx context.Context, rev string) (commit, error) {
	sisoLogURL := fmt.Sprintf("https://chromium.googlesource.com/infra/infra/+log/%s/go/src/infra/build/siso?format=JSON", rev)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sisoLogURL, nil)
	if err != nil {
		return commit{}, nil
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return commit{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return commit{}, fmt.Errorf("http=%d %s", resp.StatusCode, resp.Status)
	}
	buf, err := io.ReadAll(resp.Body)
	if err != nil {
		return commit{}, err
	}
	buf = bytes.TrimPrefix(buf, []byte(")]}'\n"))

	type userInfo struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Time  string `json:"time"`
	}
	type commitInfo struct {
		Commit  string   `json:"commit"`
		Author  userInfo `json:"author"`
		Message string   `json:"message"`
	}
	type logInfo struct {
		Log []commitInfo `json:"log"`
	}
	var commitLog logInfo
	err = json.Unmarshal(buf, &commitLog)
	if err != nil {
		return commit{}, fmt.Errorf("%w\n%s", err, buf)
	}
	if len(commitLog.Log) == 0 {
		return commit{}, fmt.Errorf("no log\n%#v\n%s", commitLog, buf)
	}
	summary := commitLog.Log[0].Message
	i := strings.IndexByte(summary, '\n')
	if i > 0 {
		summary = summary[:i]
	}
	dateStr := commitLog.Log[0].Author.Time
	// "Tue May 09 04:38:13 2023
	date, err := time.Parse("Mon Jan 02 15:04:06 2006", dateStr)
	if err != nil {
		return commit{}, err
	}
	r := commit{
		revision: commitLog.Log[0].Commit,
		summary:  summary,
		author:   commitLog.Log[0].Author.Email,
		date:     date,
	}
	return r, nil
}
