// Copyright © 2016 NAME HERE <EMAIL ADDRESS>
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cmd

import (
	"fmt"

	"github.com/dougEfresh/go-jira"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"gopkg.in/dougEfresh/gtoggl.v8"
	"gopkg.in/dougEfresh/toggl-http-client.v8"
	"gopkg.in/dougEfresh/toggl-project.v8"
	"gopkg.in/dougEfresh/toggl-timeentry.v8"
	"net/http"
	"net/http/httputil"
	"os"
	"strconv"
	"strings"
	"time"
)

const EpicField = "customfield_10450"

var togglProjects = make(map[uint64]*gproject.Project)

// syncCmd represents the sync command
var syncCmd = &cobra.Command{
	Use:   "sync",
	Short: "jira worklog to toggl time-entry",
	Long:  ``,
	Run:   run,
}

func run(cmd *cobra.Command, args []string) {
	debugClient := &BasicClient{debug: viper.GetBool("debug")}
	httpClient := &http.Client{Transport: debugClient}
	jc, err := jira.NewClient(httpClient, viper.GetString("jira.host"))
	if err != nil {
		panic(err)
	}
	logger := Debugger{Debug: viper.GetBool("debug")}
	tc, err := gtoggl.NewClient(viper.GetString("toggl.token"), ghttp.SetTraceLogger(logger))
	if err != nil {
		panic(err)
	}
	p := cmd.Flags().Lookup("password")
	if p == nil || p.Value.String() == "" {
		fmt.Fprintf(os.Stderr, "Need a password!\n")
		os.Exit(-1)
	}
	_, err = jc.Authentication.AcquireSessionCookie(viper.GetString("jira.user"), p.Value.String())
	if err != nil {
		panic(err)
	}

	w := &Worker{user: viper.GetString("jira.user"), jc: jc, tc: tc, debug: false, workspace: viper.GetString("toggl.workspace")}
	sr, err := jc.Issue.Search(viper.GetString("jira.jql"))
	if err != nil {
		panic(err)
	}
	results := make([]JiraToggl, 20, 100)
	for _, value := range sr.Issues {
		if value == nil {
			continue
		}
		fmt.Printf("%+s\n",value.Key)
		is, _, err := jc.Issue.Get(value.Key)
		if err != nil {
			panic(err)
		}
		epic := w.getEpic(is)
		w.defaultProject = w.getTogglProject(epic)
		for _, value := range w.processIssue(is, is,cmd) {
			results = append(results,value)
		}
		for _, value := range is.Fields.SubTasks {
			//fmt.Printf("%v\n",value)
			st, _, _ := jc.Issue.Get(value.Key)
			for _, value := range w.processIssue(is, st,cmd) {
				results = append(results,value)
			}

		}
	}
	table := tablewriter.NewWriter(os.Stdout)
	table.SetHeader([]string{"Jira", "Story", "Toggl"})
	for _, value := range results {
		if value.timeEntry != nil {
			table.Append([]string{value.jira.Key, value.jira.Fields.Summary, strconv.FormatUint(value.timeEntry.Duration,10)})
		}
	}
	table.Render()
}

func init() {
	RootCmd.AddCommand(syncCmd)
}

type Debugger struct {
	Debug bool
}

func (d Debugger) Printf(format string, v ...interface{}) {
	if d.Debug {
		fmt.Printf(format, v)
	}
}

type Worker struct {
	user           string
	tc             *gtoggl.TogglClient
	jc             *jira.Client
	debug          bool
	defaultProject *gproject.Project
	workspace      string
}

type JiraToggl struct {
	timeEntry *gtimeentry.TimeEntry
	jira      *jira.Issue
	worklog  *jira.Worklog
}

func (w *Worker) processIssue(issue *jira.Issue, subtask *jira.Issue,cmd *cobra.Command) []JiraToggl {
	p := w.getTogglProject(subtask)
	if p == nil {
		p = w.getTogglProject(issue)
		if p == nil {
			p = w.defaultProject
		}
	}
	results := make([]JiraToggl, 20, 100)
	for _, wl := range subtask.Fields.WorklogPage.Worklogs {
		if !strings.Contains(wl.Author.Name, w.user) {
			continue
		}
		fmt.Printf("-----Processing %s -------\n", subtask.Key)
		te, update := w.getTimeEntry(wl, issue, subtask)
		var err error
		if !update {
			results = append(results, JiraToggl{timeEntry: te, jira: subtask, worklog: wl})
		}

		dryRun , _ := cmd.Flags().GetBool("dry-run")
		if te.Id == 0 {
			if !dryRun {
				fmt.Printf("updating %+v\n", te)
				te, err = w.tc.TimeentryClient.Create(te)
			}
		} else {
			if !dryRun {
				fmt.Printf("updating %+v\n", te)
				te, err = w.tc.TimeentryClient.Update(te)
			}
		}
		if err != nil {
			panic(err)
		}
		results = append(results, JiraToggl{timeEntry: te, jira:subtask, worklog:wl})
	}
	return results
}

func (w *Worker) getTogglProject(issue *jira.Issue) *gproject.Project {
	tc := w.tc
	for _, value := range issue.Fields.Labels {
		if !strings.Contains(value, "toggl_proj=") {
			continue
		}
		tid, err := strconv.ParseUint(strings.Replace(value, "toggl_proj=", "", 1), 10, 64)
		if err != nil {
			panic(err)
		}
		var togglP *gproject.Project
		if ok := togglProjects[tid]; ok == nil {
			togglP, err = tc.ProjectClient.Get(tid)
			if err != nil {
				panic(err)
			}
			togglProjects[tid] = togglP
		} else {
			togglP = togglProjects[tid]
		}
		return togglP
	}
	return nil
}

func (w *Worker) getEpic(issue *jira.Issue) *jira.Issue {
	c := w.jc
	cf, _, err := c.Issue.GetCustomFields(issue.Key)
	if err != nil {
		panic(err)
	}
	i, _, err := c.Issue.Get(cf[EpicField])
	if err != nil {
		panic(err)
	}
	return i
}

func (w *Worker) getTimeEntry(wl *jira.Worklog, issue *jira.Issue, subtask *jira.Issue) (*gtimeentry.TimeEntry, bool) {
	tc := w.tc
	found := strings.Contains(wl.Comment, "-----tid:")
	if !found {
		return w.addNew(wl, issue, subtask), true
	}
	for _, c := range strings.Split(wl.Comment, "\n") {
		if !strings.Contains(wl.Comment, "-----tid:") {
			continue
		}
		id, err := strconv.ParseUint(strings.Replace(strings.Replace(c, "-----tid:", "", 1), "----", "", 1), 10, 65)
		if err != nil {
			panic(err)
		}
		te, err := tc.TimeentryClient.Get(id)
		if err != nil {
			panic(err)
		}
		update := false
		if wl.TimeSpentSeconds != te.Duration {
			te.Duration = wl.TimeSpentSeconds
			update = true
		}
		if !time.Time(wl.Started).Equal(te.Start) {
			te.Start = time.Time(wl.Started)
			update = true
		}

		if update {
			te = w.addNew(wl, issue, subtask)
			te.Id = id
		}
		return te, update
	}
	return nil, false
}

func (w *Worker) addNew(wl *jira.Worklog, issue *jira.Issue, subtask *jira.Issue) *gtimeentry.TimeEntry {
	i := gtimeentry.TimeEntry{}
	i.Start = time.Time(wl.Started)
	i.Stop = i.Start.Add(time.Duration(wl.TimeSpentSeconds) * time.Second)
	i.Duration = wl.TimeSpentSeconds
	i.Pid = w.defaultProject.Id
	i.Wid = w.defaultProject.WId
	if len(w.workspace) > 0 {
		id, err := strconv.ParseUint(w.workspace, 10, 64)
		if err != nil {
			panic(err)
		}
		i.Wid = id
	}
	i.CreatedWith = wl.Self
	i.Description = fmt.Sprintf("%s - %s", subtask.Key, issue.Fields.Summary)
	defaultTag := strings.Replace(fmt.Sprintf("INT_%s", issue.Fields.Type.Name), "INT_Story", "INT_Development", 1)
	i.Tags = []string{defaultTag}
	for _, value := range subtask.Fields.Labels {
		if strings.Contains(value, "toggl_tag=") {
			i.Tags = []string{strings.Replace(value, "toggl_tag=", "", 1)}
		}
	}
	return &i
}

type BasicClient struct {
	debug bool
}

func (c *BasicClient) RoundTrip(req *http.Request) (*http.Response, error) {

	_, err := httputil.DumpRequest(req, true)
	if err == nil {
		if c.debug {
			//fmt.Printf("%s\n", string(out))
		}
	}
	r, err := http.DefaultTransport.RoundTrip(req)
	_, _ = httputil.DumpResponse(r, true)
	if c.debug {
		//fmt.Printf("%s\n", string(out))
	}
	return r, err

}
