// SMPatch
// I patch live ScoreMaster installations
package main

/***************************************************************************
MIT License

Copyright (c) 2022 Bob Stammers

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.package main
*****************************************************************************/

import (
	"archive/zip"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/manifoldco/promptui"
	_ "github.com/mattn/go-sqlite3"
	yaml "gopkg.in/yaml.v2"
)

const progdesc = `
I patch (upgrade) live ScoreMaster installations.`

var verbose = flag.Bool("v", false, "Verbose")
var silent = flag.Bool("s", false, "Silent")
var force = flag.Bool("force", false, "Apply patch regardless of criteria")
var showusage = flag.Bool("?", false, "Show this help text")
var path2root = flag.String("sm", ".", "Path of ScoreMaster root folder")
var patchfile = flag.String("pf", "smpatch.zip", "File containing patches")
var dontDeletePatchfile = flag.Bool("save", false, "Don't delete the patchfile on completion")

const apptitle = "SMPatch"
const appversion = "1.1"
const timefmt = time.RFC3339

var dbh *sql.DB
var ptz *zip.ReadCloser

var cfg struct {
	Path2DB    string
	RallyTitle string
	DBVersion  int
	AppVersion string
	PatchCfg   struct {
		PatchID string   `yaml:"id"`
		MinDB   int      `yaml:"mindb"`
		MinApp  string   `yaml:"minapp"`
		MaxDB   int      `yaml:"maxdb"`
		MaxApp  string   `yaml:"maxapp"`
		Files   []string `yaml:"files"`
		SQL     []string `yaml:"sql"`
		Folders []string `yaml:"folders"`
	}
}

// fourFields: this contains the results of parsing the Subject line.
// The "four fields" are entrant, bonus, odo & claimtime
type fourFields struct {
	ok         bool
	EntrantID  int
	BonusID    string
	OdoReading int
	TimeHH     int
	TimeMM     int
	Extra      string
}

const myTimeFormat = "2006-01-02 15:04:05"

type timestamp struct {
	date time.Time
}

func appTargetVersion() string {

	if cfg.PatchCfg.MinApp == cfg.PatchCfg.MaxApp {
		return fmt.Sprintf("%v", cfg.PatchCfg.MinApp)
	}
	return fmt.Sprintf("%v-%v", cfg.PatchCfg.MinApp, cfg.PatchCfg.MaxApp)
}

func checkAppVersion() {

	v1, err := version.NewVersion(strings.ReplaceAll(cfg.AppVersion, " ", "-"))
	if err != nil {
		return
	}
	vmin, minerr := version.NewVersion(strings.ReplaceAll(cfg.PatchCfg.MinApp, " ", "-"))
	vmax, maxerr := version.NewVersion(strings.ReplaceAll(cfg.PatchCfg.MaxApp, " ", "-"))
	if *verbose {
		fmt.Printf("Vapp IS '%v' [%v]\n", v1, cfg.AppVersion)

		fmt.Printf("Vmin IS '%v' [%v]\n", vmin, cfg.PatchCfg.MinApp)

		fmt.Printf("Vmax IS '%v' [%v]\n", vmax, cfg.PatchCfg.MaxApp)

	}
	if minerr == nil && v1.LessThan(vmin) {
		fmt.Printf("AppVersion is older than %v - run aborted\n", appTargetVersion())
		osExit(1)
	}
	if maxerr == nil && v1.GreaterThan(vmax) {
		fmt.Printf("AppVersion is newer than %v - run aborted\n", appTargetVersion())
		osExit(1)
	}

}

func closePatchfile() {

	ptz.Close()
	if !*dontDeletePatchfile {
		os.Remove(*patchfile)
	}
}

func dbTargetVersion() string {

	if cfg.PatchCfg.MaxDB == cfg.PatchCfg.MinDB {
		return fmt.Sprintf("%v", cfg.PatchCfg.MaxDB)
	}
	return fmt.Sprintf("in range %v-%v", cfg.PatchCfg.MinDB, cfg.PatchCfg.MaxDB)
}

func extractTime(s string) string {
	x := strings.Split(s, ";")
	if len(x) < 1 {
		return ""
	}
	return strings.Trim(x[len(x)-1], " ")
}

func parseTime(s string) time.Time {
	//fmt.Printf("Parsing time from [ %v ]\n", s)
	if s == "" {
		return time.Time{}
	}

	formats := []string{
		time.RFC1123Z,
		"Mon, 2 Jan 2006 15:04:05 -0700",
		time.RFC1123Z + " (MST)",
		"Mon, 2 Jan 2006 15:04:05 -0700 (MST)",
	}

	for _, format := range formats {
		t, err := time.Parse(format, s)
		if err == nil {
			//fmt.Printf("Found time\n")
			return t
		}
		//fmt.Printf("Err: %v\n", err)
	}

	return time.Time{}
}

func fetchConfigFromDB() string {
	rows, err := dbh.Query("SELECT ebcsettings FROM rallyparams")
	if err != nil {
		fmt.Printf("%s can't fetch config from database [%v] run aborted\n", logts(), err)
		osExit(1)
	}
	defer rows.Close()
	rows.Next()
	var res string
	rows.Scan(&res)
	return res

}

func getYN(prompt string) bool {

	promptx := promptui.Select{
		Label: prompt,
		Items: []string{"Yes", "No"},
	}

	_, result, _ := promptx.Run()

	fmt.Printf("You chose %v\n", result)
	return result == "Yes"

}

func init() {

	flag.Usage = func() {
		w := flag.CommandLine.Output()
		fmt.Fprintf(w, "%v v%v\n", apptitle, appversion)
		flag.PrintDefaults()
		fmt.Fprintf(w, "%v\n", progdesc)
	}
	flag.Parse()
	if *showusage {
		flag.Usage()
		os.Exit(1)
	}

	if *path2root == "" {
		fmt.Printf("%s No ScoreMaster installation has been specified Run aborted\n", apptitle)
		osExit(1)
	}

	openPatchfile()

	cfg.Path2DB = filepath.Join(*path2root, "sm", "ScoreMaster.db")

	openDB(cfg.Path2DB)

	if !loadRallyData() {
		osExit(1)
	}
}

func loadRallyData() bool {

	rows, err := dbh.Query("SELECT RallyTitle, DBVersion FROM rallyparams")
	if err != nil {
		fmt.Printf("%s: OMG %v\n", apptitle, err)
		osExit(1)
	}
	defer rows.Close()
	rows.Next()

	rows.Scan(&cfg.RallyTitle, &cfg.DBVersion)

	aboutfile := filepath.Join(*path2root, "sm", "about.php")
	if _, err := os.Stat(aboutfile); os.IsNotExist(err) {
		wd, _ := os.Getwd()
		fmt.Printf("%s: Can't access %v [%v], run aborted\n", apptitle, aboutfile, wd)
		osExit(1)
	}

	file, err := os.Open(aboutfile)
	if err == nil {

		defer file.Close()

		about, _ := ioutil.ReadAll(file)
		re := regexp.MustCompile(`"version" => "([^"]+)`)
		match := re.FindStringSubmatch(string(about))
		cfg.AppVersion = match[1]

	}
	return true

}

func logts() string {

	var t = time.Now()
	return t.Format("2006-01-02 15:04:05")

}

func main() {
	if !*silent {
		fmt.Printf("%v: v%v   Copyright (c) 2022 Bob Stammers\n%v\n", apptitle, appversion, progdesc)
	}
	if !*silent {
		fmt.Printf("\nPatching \"%v\" (%v) - DBVersion is %v; AppVersion is %v\n\n", cfg.RallyTitle, *path2root, cfg.DBVersion, cfg.AppVersion)
	}
	defer closePatchfile()

	if !*force {
		if cfg.DBVersion < cfg.PatchCfg.MinDB || cfg.DBVersion > cfg.PatchCfg.MaxDB {
			fmt.Printf("DBVersion is not %v - run aborted\n", dbTargetVersion())
			osExit(1)
		}
		checkAppVersion()
	} else {
		if !*silent {
			fmt.Println("Forcing patch application")
		}
	}
	if !*silent {
		fmt.Printf("\nApplying patch \"%v\"\n\n", cfg.PatchCfg.PatchID)
		if !getYN("Ok to apply this patch") {
			fmt.Println("Run abandoned")
			osExit(1)
		}
	}
	runPatchSQL()
	runMakeFolders()
	runFileCopies()

	if !*silent {
		fmt.Printf("Patch applied successfully\n\n")
		closePatchfile()
		osExit(0)
	}

}

func openDB(dbpath string) {

	var err error
	if _, err = os.Stat(dbpath); errors.Is(err, os.ErrNotExist) {
		fmt.Printf("%v: Cannot access database %v [%v] run aborted\n", apptitle, dbpath, err)
		osExit(1)
	}

	dbh, err = sql.Open("sqlite3", dbpath)
	if err != nil {
		fmt.Printf("%v: Can't access database %v [%v] run aborted\n", apptitle, dbpath, err)
		osExit(1)
	}

}

func openPatchfile() {

	var err error

	ptz, err = zip.OpenReader(*patchfile)
	if err != nil {
		fmt.Printf("%v: Can't access patchfile %v [%v] run aborted\n", apptitle, *patchfile, err)
		osExit(1)
	}
	r, err := ptz.Open("smpatch.yml")
	if err != nil {
		fmt.Printf("%v: Patchfile is malformed - run aborted\n", apptitle)
		osExit(1)
	}
	defer r.Close()
	D := yaml.NewDecoder(r)
	D.Decode(&cfg.PatchCfg)

}

func osExit(res int) {

	if !*silent {
		waitforkey()
	}

	defer os.Exit(res)
	runtime.Goexit()

}

func runFileCopies() {

	copyFiles := len(cfg.PatchCfg.Files) > 0
	if copyFiles {
		fmt.Println("Updating application files")
	}
	for _, line := range cfg.PatchCfg.Files {
		if *verbose {
			fmt.Printf("Updating %v\n", line)
		}

		x := strings.ReplaceAll(line, "/", string(filepath.Separator))
		y := filepath.Join(*path2root, x)
		z := filepath.Base(y)
		if *verbose {
			fmt.Printf("Writing %v\n", y)
		}

		rc, err := ptz.Open(z)
		if err != nil {
			fmt.Printf("*** Can't read patch %v [%v]\n", line, err)
			continue
		}
		f, err := os.Create(y)
		if err != nil {
			fmt.Printf("*** Can't create file %v [%v]\n", y, err)
			continue
		}
		io.Copy(f, rc)
		f.Close()

		rc.Close()
	}
	if copyFiles {
		fmt.Println("File patches applied")
	}

}

func runMakeFolders() {

	for _, line := range cfg.PatchCfg.Folders {
		if *verbose {
			fmt.Printf("Making folder %v\n", line)
		}
		x := strings.ReplaceAll(line, "/", string(filepath.Separator))
		y := filepath.Join(*path2root, x)
		err := os.MkdirAll(y, os.ModeDir)
		if err != nil {
			fmt.Printf("*** %v ** FAILED ** %v\n", line, err)
		}

	}

}

func runPatchSQL() {

	applyPatch := len(cfg.PatchCfg.SQL) > 0
	if applyPatch {
		fmt.Println("Upgrading the database")
	}
	for _, line := range cfg.PatchCfg.SQL {
		if *verbose {
			fmt.Printf("Applying %v\n", line)
		}
		_, err := dbh.Exec(line)
		if err != nil {
			fmt.Printf("*** %v ** FAILED ** %v\n", line, err)
		}

	}
	if applyPatch {
		fmt.Println("Database upgraded")
	}
}
func waitforkey() {

	fmt.Printf("\n%v: Press [Enter] to exit ... \n", apptitle)
	fmt.Scanln()

}
