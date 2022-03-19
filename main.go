// SMPatch
// I patch live ScoreMaster installations
package main

import (
	"archive/zip"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	_ "github.com/mattn/go-sqlite3"
	yaml "gopkg.in/yaml.v2"
)

const progdesc = `
I patch live ScoreMaster installations.`

var verbose = flag.Bool("v", false, "Verbose")
var silent = flag.Bool("s", false, "Silent")
var showusage = flag.Bool("?", false, "Show this help text")
var path2root = flag.String("sm", ".", "Path of ScoreMaster root folder")
var patchfile = flag.String("pf", "smpatch.zip", "File containing patches")
var debugwait = flag.Bool("dw", false, "Wait for [Enter] at exit (debug)")
var dontDeletePatchfile = flag.Bool("save", false, "Don't delete the patchfile on completion")

const apptitle = "SMPatch"
const appversion = "1.0"
const timefmt = time.RFC3339

var dbh *sql.DB
var ptz *zip.ReadCloser

var cfg struct {
	Path2DB    string
	KeyWait    bool
	RallyTitle string
	DBVersion  int
	AppVersion string
	PatchCfg   struct {
		PatchID string `yaml:"id"`
		MinDB   int    `yaml:"mindb"`
		MinApp  string `yaml:"minapp"`
		MaxDB   int    `yaml:"maxdb"`
		MaxApp  string `yaml:"maxapp"`
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

func checkAppVersion() {

	v1, _ := version.NewVersion(strings.ReplaceAll(cfg.AppVersion, " ", "-"))
	vmin, _ := version.NewVersion(strings.ReplaceAll(cfg.PatchCfg.MinApp, " ", "-"))
	vmax, _ := version.NewVersion(strings.ReplaceAll(cfg.PatchCfg.MaxApp, " ", "-"))
	if *verbose {
		fmt.Printf("Vapp IS '%v' [%v]\n", v1, cfg.AppVersion)
		fmt.Printf("Vmin IS '%v' [%v]\n", vmin, cfg.PatchCfg.MinApp)
		fmt.Printf("Vmax IS '%v' [%v]\n", vmax, cfg.PatchCfg.MaxApp)
	}
	if v1.LessThan(vmin) {
		fmt.Printf("AppVersion is older than range %v-%v - run aborted\n", cfg.PatchCfg.MinApp, cfg.PatchCfg.MaxApp)
		osExit(1)
	}
	if v1.GreaterThan(vmax) {
		fmt.Printf("AppVersion is newer than range %v-%v - run aborted\n", cfg.PatchCfg.MinApp, cfg.PatchCfg.MaxApp)
		osExit(1)
	}

}

func closePatchfile() {

	ptz.Close()
	if !*dontDeletePatchfile {
		os.Remove(*patchfile)
	}
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

func init() {

	//ex, _ := os.Executable()
	//exPath := filepath.Dir(ex)
	//os.Chdir(exPath)

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

	defer closePatchfile()

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
		fmt.Printf("%v: v%v   Copyright (c) 2022 Bob Stammers\n", apptitle, appversion)
	}
	if !*silent {
		fmt.Printf("Patching \"%v\" (%v) - DBVersion is %v; AppVersion is %v\n", cfg.RallyTitle, *path2root, cfg.DBVersion, cfg.AppVersion)
	}
	if cfg.DBVersion < cfg.PatchCfg.MinDB || cfg.DBVersion > cfg.PatchCfg.MaxDB {
		fmt.Printf("DBVersion is not in range %v-%v - run aborted\n", cfg.PatchCfg.MinDB, cfg.PatchCfg.MaxDB)
		osExit(1)
	}
	checkAppVersion()
	if !*silent {
		fmt.Printf("Applying patch \"%v\"\n", cfg.PatchCfg.PatchID)
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

	if *debugwait || cfg.KeyWait {
		waitforkey()
	}

	defer os.Exit(res)
	runtime.Goexit()

}

func waitforkey() {

	fmt.Printf("%v: Press [Enter] to exit ... \n", apptitle)
	fmt.Scanln()

}
