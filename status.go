package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"
)

// runStatus is `claude-devin-acp status`: the Devin tasks a running `serve` has, or what one of
// them has done, followed as it goes on with -f. For a look at them from anywhere a shell reaches,
// as over SSH.
func runStatus(args []string) {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	addr := flags.String("addr", envOr("CLAUDE_DEVIN_SERVE", "127.0.0.1:47821"), "the address serve listens on")
	follow := flags.Bool("f", false, "with a session: keep printing what it does")
	lines := flags.Int("n", 40, "with a session: how many of its latest lines to print")
	flags.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: claude-devin-acp status [-f] [-n lines] [session]\n\nWith no session, the tasks running and those finished in the last day. A session is\nOpenCode's id, Devin's, or the start of either.")
		flags.PrintDefaults()
	}
	_ = flags.Parse(args)
	base := "http://" + *addr + "/status"
	if flags.NArg() == 0 {
		var tasks []task
		if err := get(base, &tasks); err != nil {
			fail(err)
		}
		if len(tasks) == 0 {
			fmt.Println("no Devin tasks running or finished in the last day")
		}
		for _, t := range tasks {
			fmt.Println(t.String())
			fmt.Println()
		}
		return
	}
	one := base + "/" + url.PathEscape(flags.Arg(0))
	var page struct {
		Task  task     `json:"task"`
		Lines []string `json:"lines"`
		Next  int      `json:"next"`
	}
	if err := get(one+"?lines="+strconv.Itoa(*lines), &page); err != nil {
		fail(err)
	}
	fmt.Println(page.Task.String())
	fmt.Println()
	for _, line := range page.Lines {
		fmt.Println(line)
	}
	for *follow {
		time.Sleep(2 * time.Second)
		if err := get(one+"?since="+strconv.Itoa(page.Next), &page); err != nil {
			fail(err)
		}
		for _, line := range page.Lines {
			fmt.Println(line)
		}
	}
}

// get reads an answer of the server's, with the token when this user can read it.
func get(address string, into any) error {
	request, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return err
	}
	if token := clientToken(); token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return fmt.Errorf("is serve running? %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var message [512]byte
		n, _ := response.Body.Read(message[:])
		return fmt.Errorf("%s: %s", response.Status, message[:n])
	}
	return json.NewDecoder(response.Body).Decode(into)
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "claude-devin-acp status:", err)
	os.Exit(1)
}
