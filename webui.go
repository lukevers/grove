package main

// Copyright ⓒ 2013 Alexander Bauer and Luke Evers (see LICENSE.md)

import (
	"encoding/base64"
	"errors"
	"github.com/russross/blackfriday"
	"html"
	"html/template"
	"io"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
)

type gitPage struct {
	Owner      string
	InRepoPath string
	URL        string
	GitDir     string
	Branch     string
	Host       string
	TagNum     string
	Path       string
	CommitNum  string
	SHA        string
	Content    template.HTML
	List       []*dirList
	Logs       []*gitLog
	Version    string
	Query      template.URL
}

type gitLog struct {
	Author    string
	Classtype string
	SHA       string
	Time      string
	Subject   template.HTML
	Body      template.HTML
}

type dirList struct {
	URL   template.URL
	Name  string
	Link  string
	Query template.URL
}

const (
	defaultRef     = "HEAD" // Default git reference
	defaultCommits = 10     // Default number of commits to show
)

var (
	internalServerError = errors.New(
		http.StatusText(http.StatusInternalServerError))
	forbidden = errors.New(
		http.StatusText(http.StatusForbidden))
)

// Check for a .git directory in the repository argument. If one does
// not exist, we will generate a directory listing, rather than a
// repository view.
func isGit(repository string) (git bool, gitDir string) {
	_, err := os.Stat(path.Join(repository, ".git"))
	if err == nil {
		// Note that err EQUALS nil
		git = true
		gitDir = "/.git"
	}
	return
}

// Retrieval of file info is done in two steps so that we can use
// os.Stat(), rather than os.Lstat(), the former of which follows
// symlinks.
func MakeDirInfos(repository string, dirnames []string) (dirinfos []os.FileInfo) {
	dirinfos = make([]os.FileInfo, 0, len(dirnames))
	for _, n := range dirnames {
		info, err := os.Stat(repository + "/" + n)
		if err == nil && CheckPerms(info) {
			dirinfos = append(dirinfos, info)
		}
	}
	return
}

// MakePage acts as a multiplexer for the various complex http
// functions. It handles logging and web error reporting.
func MakePage(w http.ResponseWriter, req *http.Request, repository string, file string, isFile bool) {
	g := &git{
		Path: repository,
	}
	// First, establish the template and fill out some of the gitPage.
	pageinfo := &gitPage{
		Owner:      gitVarUser(),
		InRepoPath: path.Join(path.Base(repository), file),
		URL:        "http://" + req.Host + strings.TrimRight(req.URL.Path, "/") + "/",
		Host:       req.Host,
		Version:    Version,
		Path:       repository[len(handler.Dir):], // URL of repository
	}
	
	// Check to see if there's a query or not
	if req.URL.RawQuery == "" {
		pageinfo.Query = template.URL(req.URL.RawQuery)
	} else {
		pageinfo.Query = template.URL("?" + req.URL.RawQuery)
	}

	// Now, check if the given directory is a git repository, and if
	// so, parse some of the possible http forms.
	var ref string
	git, gitDir := isGit(repository)
	if git {
		// ref is the git commit reference. If the form is not submitted,
		// (or is invalid), it is set to "HEAD".
		ref = req.FormValue("ref")
		if len(ref) == 0 || !g.RefExists(ref) {
			ref = "HEAD" // The commit or branch reference
		}

		// The form value since is just a shortcut for
		// "?ref=<ref>..<since>", so we check it here. Note that the
		// results will include <ref> and exclude <since>.
		if since := req.FormValue("since"); g.RefExists(since) {
			ref = since + ".." + ref
		}

		pageinfo.Branch = g.Branch("HEAD")
		pageinfo.TagNum = strconv.Itoa(len(g.Tags()))
		pageinfo.CommitNum = strconv.Itoa(g.TotalCommits())
		pageinfo.SHA = g.SHA(ref)
		pageinfo.GitDir = gitDir
	}

	// TODO: all of the below case blocks may misbehave if the URL
	// contains a keyword.
	var err error
	var status int
	switch {
	case !git:
		// This will catch all non-git cases, eliminating the need for
		// them below.
		err, status = MakeDirPage(w, pageinfo, req, repository)
	case strings.Contains(pageinfo.URL, "tree"):
		// This will catch cases needing to serve directories within
		// git repositories.
		err, status = MakeTreePage(w, req, pageinfo, g, ref, file)
	case strings.Contains(pageinfo.URL, "blob"):
		// This will catch cases needing to serve files.
		err, status = MakeFilePage(w, pageinfo, g, ref, file)
	case strings.Contains(pageinfo.URL, "raw"):
		// This will catch cases needing to serve files directly.
		err, status = MakeRawPage(w, file, ref, g)
	case git:
		// This will catch cases serving the main page of a repository
		// directory. This needs to be last because the above cases
		// for "tree" and "blob" will also have `git` as true.
		err, status = MakeGitPage(w, req, pageinfo, g, ref, file)
	}

	// If an error was encountered, ensure that an error page is
	// displayed, then close the connection and return.
	if err != nil {
		l.Errf("View of %q from %q caused error: %s",
			pageinfo.Path, req.RemoteAddr, err)
		Error(w, status)
	} else {
		l.Debugf("View of %q from %q\n",
			pageinfo.Path, req.RemoteAddr)
	}
}

// Error reports an error of the given status to the given http
// connection using http.StatusText().
func Error(w http.ResponseWriter, status int) {
	// TODO: use templates to give informative error pages.
	http.Error(w, strconv.Itoa(status)+" - "+http.StatusText(status),
		status)
}

// MakeRawPAge makes the raw page of which the files are shown as
// completely raw files.
func MakeRawPage(w io.Writer, file, ref string, g *git) (err error, status int) {
	_, err = w.Write(g.GetFile(ref, file))
	return err, http.StatusOK
}

// MakeDirPage makes filesystem directory listings, which are not
// contained within git projects. It writes the webpage to the
// provided http.ResponseWriter.
func MakeDirPage(w http.ResponseWriter, pageinfo *gitPage,
	req *http.Request, directory string) (err error, status int) {

	// First, check the permissions of the file to be displayed.
	fi, err := os.Stat(directory)
	if err != nil {
		return err, http.StatusNotFound
	}
	if !CheckPerms(fi) {
		return forbidden, http.StatusForbidden
	}
	// We only get beyond this point if we are allowed to serve the
	// directory.

	// We begin the template here so that we can fill it out.

	pageinfo.List = make([]*dirList, 0, 2)
	if pageinfo.Path != "" {
		// If we're not on the root directory, we need two links for
		// navigation: "/" and ".."
		pageinfo.List = append(pageinfo.List,
			&dirList{ // append "/"
				URL:  template.URL("/"),
				Name: "/",
			}, &dirList{ // and append ".."
				URL:  template.URL(pageinfo.URL + "../"),
				Name: "..",
			})
	} else {
		pageinfo.Path = "/"
	}

	// Open the file so that it can be read.
	f, err := os.Open(directory)
	if err != nil || f == nil {
		// If there is an error opening the file, return 500.
		l.Errf("View of %q from %q caused error: %s",
			pageinfo.Path, req.RemoteAddr, err)
		Error(w, http.StatusNotFound)
		return
	}

	// To list the directory properly, we have to do it in two
	// steps. First, retrieve the names, then perform os.Stat() on the
	// result. This is so that simlinks are followed. We will also
	// check file permissions.
	dirnames, err := f.Readdirnames(0)
	f.Close()
	if err != nil {
		// If the directory could not be opened, return 500.
		l.Errf("View of %q from %q caused error: %s",
			pageinfo.Path, req.RemoteAddr, err)
		Error(w, http.StatusInternalServerError)
		return
	}
	// We have the directory names; go on to calling os.Stat() and
	// checking their permissions. If they should be listed, add
	// them to a buffer, then append that to the dirlist at the
	// end.
	dirbuf := make([]*dirList, 0, len(dirnames))
	for _, n := range dirnames {
		info, err := os.Stat(directory + "/" + n)
		if err == nil && CheckPerms(info) {
			dirbuf = append(dirbuf, &dirList{
				URL:  template.URL(pageinfo.URL + info.Name() + "/"),
				Name: info.Name(),
			})

		}
	}
	pageinfo.List = append(pageinfo.List, dirbuf...)

	// We return 500 here because the error will only be reported
	// if t.ExecuteTemplate() results in an error.
	return t.ExecuteTemplate(w, "dir.html", pageinfo),
		http.StatusInternalServerError
}

// MakeFilePage shows the contents of a file within a git project. It
// writes the webpage to the provided io.Writer.
func MakeFilePage(w io.Writer, pageinfo *gitPage,
	g *git, ref string, file string) (err error, status int) {
	// First we need to get the content,
	pageinfo.Content = template.HTML(string(g.GetFile(ref, file)))
	// then we need to figure out how many lines there are.
	lines := strings.Count(string(pageinfo.Content), "\n")
	// For each of the lines, we want to prepend
	//    <div id=\"L-"+j+"\">
	// and append
	//    </div>
	// Also, we want to add line numbers.
	temp := ""
	temp_html := ""
	temp_content := strings.SplitAfter(string(pageinfo.Content), "\n")

	// Image support
	if extention := path.Ext(file); extention == ".png" ||
		extention == ".jpg" ||
		extention == ".jpeg" ||
		extention == ".gif" {

		var image []byte = []byte(pageinfo.Content)
		img := base64.StdEncoding.EncodeToString(image)
		temp_html = "<img src=\"data:image/" + strings.TrimLeft(extention, ".") + ";base64," + img + "\"/>"
	} else {
		for j := 1; j <= lines+1; j++ {
			temp_html += "<div id=\"L-" + strconv.Itoa(j) + "\">" +
				html.EscapeString(temp_content[j-1]) + "</div>"
			temp += "<a href=\"#L-" + strconv.Itoa(j) + "\" class=\"line\">" +
				strconv.Itoa(j) + "</a><br/>"
		}
	}

	pageinfo.Content = template.HTML(temp_html)

	// We return 500 here because the error will only be reported
	// if t.ExecuteTemplate() results in an error.
	return t.ExecuteTemplate(w, "file.html", pageinfo),
		http.StatusInternalServerError

}

// MakeGitPage shows the "front page" that is the main directory of a
// git reposiory, including the README and a directory listing. It
// writes the webpage to the provided io.Writer.
func MakeGitPage(w http.ResponseWriter, req *http.Request, pageinfo *gitPage, g *git, ref, file string) (err error, status int) {

	// To begin with, parse the remaining portions of the http form.

	// maxCommits is the maximum number of commits to be loaded via
	// the log.
	maxCommits, err := strconv.Atoi(req.FormValue("c"))
	if err != nil {
		maxCommits = 10
	}

	// useAPI is a boolean indicator of whether or not to use the
	// API. It must be retrieved by direct access to req.Form because
	// the form can be empty. (In this case, we would fall back to
	// checking the Accept field in the header.)
	if _, useAPI := req.Form["api"]; useAPI {
		err = ServeAPI(w, req, g, ref, maxCommits)
		if err != nil {
			l.Errf("API request %q from %q failed: %s",
				req.URL, req.RemoteAddr, err)
		} else {
			l.Debugf("API request %q from %q\n",
				req.URL, req.RemoteAddr)
		}
	}

	// Parse the log to retrieve the commits.
	commits := g.Commits(ref, maxCommits)

	pageinfo.Logs = make([]*gitLog, len(commits))
	for i, c := range commits {
		if len(c.SHA) == 0 {
			// If, for some reason, the commit doesn't have content,
			// skip it.
			continue
		}
		var classtype string
		if c.Author == pageinfo.Owner {
			classtype = "-owner"
		}

		pageinfo.Logs[i] = &gitLog{
			Author:    c.Author,
			Classtype: classtype,
			SHA:       c.SHA,
			Time:      c.Time,
			Subject:   template.HTML(html.EscapeString(c.Subject)),
			Body:      template.HTML(strings.Replace(html.EscapeString(c.Body), "\n", "<br/>", -1)),
		}
	}

	if len(file) == 0 {
		// Load the README if it can be located. To locate, go through
		// a list of possible names and break the loop at the first
		// one.
		for _, fn := range []string{"README", "README.txt", "README.md"} {
			readme := g.GetFile(ref, fn)
			if len(readme) != 0 {
				pageinfo.Content = template.HTML(
					blackfriday.MarkdownCommon(readme))
				break
			}
		}
	}

	// We return 500 here because the error will only be reported
	// if t.ExecuteTemplate() results in an error.
	return t.ExecuteTemplate(w, "gitpage.html", pageinfo),
		http.StatusInternalServerError
}

// MakeTreePage makes directory listings from within git repositories.
// It writes the webpage to the provided http.ResponseWriter.
func MakeTreePage(w http.ResponseWriter, req *http.Request, pageinfo *gitPage, g *git, ref, file string) (err error, status int) {
	if strings.HasSuffix(file, "/") {
		files := g.GetDir(ref, file)
		pageinfo.List = make([]*dirList, len(files))
		for n, f := range files {
			d := &dirList{
				URL:  template.URL(f),
				Name: f,
			}

			if req.URL.RawQuery == "" {
				d.Query = template.URL(req.URL.RawQuery)
			} else {
				d.Query = template.URL("?" + req.URL.RawQuery)
			}

			var t string
			if strings.HasSuffix(f, "/") {
				t = "tree"
			} else {
				t = "blob"
			}
			d.Link = "http://" + req.Host + pageinfo.Path + "/" + t + "/" + path.Join(file, f)
			pageinfo.List[n] = d
		}
	}
	// We return 500 here because the error will only be reported
	// if t.ExecuteTemplate() results in an error.
	return t.ExecuteTemplate(w, "tree.html", pageinfo),
		http.StatusInternalServerError
}
