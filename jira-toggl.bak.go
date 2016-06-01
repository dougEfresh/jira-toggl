package main

import (
	"flag"
	"fmt"
	"github.com/dougEfresh/go-jira"
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

var (
	togglProjects = make(map[uint64]*gproject.Project)
	envToggl, _   = os.LookupEnv("TOGGL_API")
)

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

var (
	pass      = flag.String("p", "", "password")
	user      = flag.String("u", "", "username")
	jiraHost  = flag.String("j", "", "Jira host")
	toggl     = flag.String("t", envToggl, "Toggl Token")
	debug     = flag.Bool("d", false, "debug")
	workspace = flag.String("w", "", "workspace id")
	query = flag.String("q", "", "jql")
)

func main() {
	flag.Parse()
	if len(*query) ==0 {
		fmt.Fprintf(os.Stderr,"Need to pass a query (-q)\n")
		flag.PrintDefaults()
		os.Exit(-1)
	}
	debugClient := &BasicClient{debug: *debug}
	httpClient := &http.Client{Transport: debugClient}
	c, err := jira.NewClient(httpClient, *jiraHost)
	if err != nil {
		panic(err)
	}
	logger := Debugger{Debug: *debug}
	tc, err := gtoggl.NewClient(*toggl, ghttp.SetTraceLogger(logger))
	if err != nil {
		panic(err)
	}
	_, err = c.Authentication.AcquireSessionCookie(*user, *pass)
	if err != nil {
		panic(err)
	}

	w := &Worker{user: *user, jc: c, tc: tc, debug: *debug, workspace: *workspace}

	sr, err := c.Issue.Search(*query)
	if err != nil {
		panic(err)
	}
	for _, value := range sr.Issues {
		//fmt.Printf("%+s\n",value.Key)
		is, _, err := c.Issue.Get(value.Key)
		if err != nil {
			panic(err)
		}
		epic := w.getEpic(is)
		w.defaultProject = w.getTogglProject(epic)
		w.processIssue(is, is)
		for _, value := range is.Fields.SubTasks {
			//fmt.Printf("%v\n",value)
			st, _, _ := c.Issue.Get(value.Key)
			w.processIssue(is, st)
		}
	}
}

func (w *Worker) processIssue(issue *jira.Issue, subtask *jira.Issue) {
	p := w.getTogglProject(subtask)
	if p == nil {
		p = w.getTogglProject(issue)
		if p == nil {
			p = w.defaultProject
		}
	}
	for _, wl := range subtask.Fields.WorklogPage.Worklogs {
		if strings.Contains(wl.Author.Name, w.user) {
			fmt.Printf("-----Processing %s -------\n", subtask.Key)
			te, update := w.getTimeEntry(wl,issue,subtask)
			var err error
			if update {
				fmt.Printf("updating %v",te)
				if te.Id == 0 {
					te,err = w.tc.TimeentryClient.Create(te)
				} else {
					te,err= w.tc.TimeentryClient.Update(te)
				}
				if err != nil {
					panic(err)
				}
			}
		}
	}
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
	return i;
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
		fmt.Printf("%d=%d\n\n",te.Duration,wl.TimeSpentSeconds)
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
		return te , update
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

	out, err := httputil.DumpRequest(req, true)
	if err == nil {
		if c.debug {
			fmt.Printf("%s\n", string(out))
		}
	}
	r, err := http.DefaultTransport.RoundTrip(req)
	out, _ = httputil.DumpResponse(r, true)
	if c.debug {
		fmt.Printf("%s\n", string(out))
	}
	return r, err

}
