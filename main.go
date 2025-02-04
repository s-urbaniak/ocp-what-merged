package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/google/go-github/github"
	"github.com/lensesio/tableprinter"
	"github.com/xhit/go-str2duration/v2"
	"github.com/xxjwxc/gowp/workpool"
	"golang.org/x/oauth2"
)

const defaultPayload = "quay.io/openshift-release-dev/ocp-release:4.9.0-fc.0-x86_64"

type Release struct {
	Refs References `json:"references"`
}

type References struct {
	Spec ReferencesSpec `json:"spec"`
}

type ReferencesSpec struct {
	Tags []Tag `json:"tags"`
}

type Tag struct {
	Name        string            `json:"name"`
	Annotations map[string]string `json:"annotations"`
}

type Change struct {
	URL     string `header:"URL"`
	Message string `header:"Message"`
	Time    string `header:"When"`

	repository   string
	originalTime time.Time
}

type ProcessOptions struct {
	Concurrency int

	Since      time.Duration
	BranchName string
}

func parseRepositoryOrgName(repository string) (string, string, bool) {
	if !strings.HasPrefix(repository, "https://github.com/") {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(repository, "https://github.com/"), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func getRepositoryChanges(ctx context.Context, client *github.Client, repository string, options ProcessOptions) ([]*github.RepositoryCommit, error) {
	organization, name, ok := parseRepositoryOrgName(repository)
	if !ok {
		return nil, fmt.Errorf("unable to parse repository organization or name: %q", repository)
	}

	commits, _, err := client.Repositories.ListCommits(ctx, organization, name, &github.CommitsListOptions{
		SHA:   options.BranchName,
		Since: time.Now().Add(-options.Since),
		// TODO: If you want to add Until, this is the place.
	})
	if err != nil {
		log.Printf("[%s] %v", repository, err)
		return []*github.RepositoryCommit{}, nil
	}
	return commits, nil
}

// this is weak, but cheap and does not require extra request to GH API
func isMergeCommit(commit *github.Commit) bool {
	return strings.Contains(commit.GetMessage(), "Merge pull request")
}

func sanitizeMessage(msg string) string {
	lines := strings.Split(msg, "\n")
	var r []string
	for _, l := range lines {
		// filter out signatures from commit messages
		if strings.Contains(l, "Signed-off-by") || len(strings.TrimSpace(l)) == 0 {
			continue
		}
		// trim the length of each line to 80 characters
		if len(l) > 80 {
			l = l[0:80] + " ..."
		}
		r = append(r, strings.TrimSpace(l))
	}
	return strings.Join(r, "\n")
}

func processRepositories(ctx context.Context, client *github.Client, options ProcessOptions, repositories []string) ([]Change, error) {
	wp := workpool.New(options.Concurrency)
	var changes []Change
	var commitsLock sync.Mutex
	var tasks []workpool.TaskHandler

	for i := range repositories {
		repository := &repositories[i]
		tasks = append(tasks, func() error {
			result, err := getRepositoryChanges(ctx, client, *repository, options)
			if err != nil {
				return err
			}
			var change []Change
			for _, c := range result {
				if isMergeCommit(c.GetCommit()) {
					continue
				}
				change = append(change, Change{
					repository:   *repository,
					URL:          c.GetHTMLURL(),
					Message:      sanitizeMessage(c.GetCommit().GetMessage()),
					Time:         humanize.Time(c.GetCommit().GetCommitter().GetDate()),
					originalTime: c.GetCommit().GetCommitter().GetDate(),
				})
			}

			commitsLock.Lock()
			defer commitsLock.Unlock()
			changes = append(changes, change...)
			return nil
		})
	}

	// schedule all tasks, the work pool will take care of queuing
	for i := range tasks {
		wp.Do(tasks[i])
	}
	if err := wp.Wait(); err != nil {
		return nil, err
	}

	// sort by time, from oldest to latest
	sort.Slice(changes, func(i, j int) bool {
		return changes[j].originalTime.After(changes[i].originalTime)
	})
	return changes, nil
}

func getRepositoriesFromPayload(payload string) ([]string, error) {
	cmd := exec.Command("sh", "-c", fmt.Sprintf("oc adm release info %s --commit-urls -o json", payload))
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, err
	}
	var release Release
	if err := json.Unmarshal(out, &release); err != nil {
		return nil, err
	}
	var repositories []string
	for _, t := range release.Refs.Spec.Tags {
		sourceLocation, ok := t.Annotations["io.openshift.build.source-location"]
		if !ok {
			continue
		}
		if len(sourceLocation) == 0 {
			continue
		}
		hasRepository := false
		for _, r := range repositories {
			if sourceLocation == r {
				hasRepository = true
				break
			}
		}
		if !hasRepository {
			repositories = append(repositories, sourceLocation)
		}
	}
	return repositories, nil
}

func main() {
	var (
		since   string
		branch  string
		payload string
	)

	flag.StringVar(&since, "since", "1d", "Relative time to search the commits from (eg. '1d', '48h', ...)")
	flag.StringVar(&branch, "branch", "master", "Branch name to use for search (eg. 'release-4.6', ...)")
	flag.StringVar(&payload, "payload", defaultPayload, "Payload URL to use to determine list of repositories")

	flag.Parse()

	githubToken := os.Getenv("GITHUB_TOKEN")
	if len(githubToken) == 0 {
		log.Fatal(":-( I need you to set GITHUB_TOKEN env variable in order to be able to talk to Github")
	}
	client := github.NewClient(oauth2.NewClient(context.TODO(), oauth2.StaticTokenSource(&oauth2.Token{AccessToken: githubToken})))

	processOptions := ProcessOptions{
		Concurrency: 10,
	}

	if len(since) > 0 {
		var err error
		processOptions.Since, err = str2duration.ParseDuration(since)
		if err != nil {
			log.Fatalf(":-( I am unable to parse duration %q", since)
		}
	}
	if len(branch) > 0 {
		processOptions.BranchName = branch
	}

	ctx := context.Background()

	repos, err := getRepositoriesFromPayload(payload)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Processing %d repositories for commits in %s branch, since %s ...", len(repos), processOptions.BranchName, processOptions.Since)
	changes, err := processRepositories(ctx, client, processOptions, repos)
	if err != nil {
		log.Fatal(err)
	}

	printer := tableprinter.New(os.Stdout)
	printer.Print(changes)
}
