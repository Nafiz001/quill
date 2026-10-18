// Command quill runs the database, either as an interactive shell or as a
// server speaking the PostgreSQL wire protocol.
//
//	quill data.db                       an interactive shell
//	quill -serve :5433 data.db          a server psql can connect to
//	quill -c "SELECT 1" data.db         one statement, then exit
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Nafiz001/quill/internal/db"
	"github.com/Nafiz001/quill/internal/exec"
	"github.com/Nafiz001/quill/internal/wire"
)

const version = "0.1.0"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "quill:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		serveAddr   string
		command     string
		poolSize    int
		noSync      bool
		readOnly    bool
		showVersion bool
		stmtTimeout time.Duration
		maxRows     int
		logLevel    string
	)
	flag.StringVar(&serveAddr, "serve", "", "serve the PostgreSQL protocol on this address, e.g. :5433")
	flag.StringVar(&command, "c", "", "run one statement and exit")
	flag.IntVar(&poolSize, "pool", 0, "buffer pool size in pages (default 1024, which is 4MB)")
	flag.BoolVar(&noSync, "no-sync", false, "do not fsync the log on commit: faster, and a crash can lose recent commits")
	flag.BoolVar(&readOnly, "read-only", false, "refuse every statement that would write")
	flag.DurationVar(&stmtTimeout, "statement-timeout", 0, "cancel a statement that runs longer than this, e.g. 5s (default: no limit)")
	flag.IntVar(&maxRows, "max-rows", 0, "refuse a query that would return more rows than this (default: no limit)")
	flag.BoolVar(&showVersion, "version", false, "print the version and exit")
	flag.StringVar(&logLevel, "loglevel", "info", "log level: debug, info, warn, error")
	flag.Parse()

	if showVersion {
		fmt.Println("quill", version)
		return nil
	}

	path := flag.Arg(0)
	if path == "" {
		return fmt.Errorf("usage: quill [flags] <database file>")
	}

	opts := db.DefaultOptions()
	opts.PoolSize = poolSize
	opts.SyncOnCommit = !noSync
	opts.ReadOnly = readOnly
	opts.StatementTimeout = stmtTimeout
	opts.MaxResultRows = maxRows

	database, err := db.Open(path, opts)
	if err != nil {
		return err
	}
	defer database.Close()

	// Recovery having done work means the last shutdown was not clean, which
	// is worth saying out loud rather than hiding.
	if stats := database.RecoveryStats(); stats != nil && stats.PagesRestored > 0 {
		fmt.Fprintf(os.Stderr,
			"quill: recovered from an unclean shutdown: %d pages restored from %d committed transactions\n",
			stats.PagesRestored, stats.Committed)
	}

	switch {
	case command != "":
		return runOnce(database, command)
	case serveAddr != "":
		return serve(database, serveAddr, logLevel)
	default:
		return repl(database, path)
	}
}

func runOnce(database *db.DB, command string) error {
	session := database.NewSession()
	results, err := session.ExecuteMany(command)
	for _, res := range results {
		printResult(os.Stdout, res)
	}
	return err
}

func serve(database *db.DB, addr, logLevel string) error {
	level, err := parseLevel(logLevel)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	server := wire.NewServer(database, addr, logger)
	if err := server.Listen(); err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "quill %s listening on %s\n", version, server.Addr())
	fmt.Fprintf(os.Stderr, "connect with: psql -h localhost -p %s -U quill\n", portOf(server.Addr().String()))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		fmt.Fprintln(os.Stderr, "\nquill: shutting down")
		// A second signal exits without waiting, in case a connection is
		// wedged.
		hard := make(chan os.Signal, 1)
		signal.Notify(hard, os.Interrupt, syscall.SIGTERM)
		select {
		case <-hard:
			os.Exit(1)
		case <-time.After(10 * time.Second):
			os.Exit(1)
		}
	}()

	return server.Serve(ctx)
}

func portOf(addr string) string {
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[i+1:]
	}
	return addr
}

// repl is the interactive shell.
func repl(database *db.DB, path string) error {
	session := database.NewSession()
	scanner := bufio.NewScanner(os.Stdin)
	// A row can be long, so the default 64KB line limit is raised.
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	fmt.Printf("quill %s -- %s\n", version, path)
	fmt.Println(`Type SQL terminated by ";". \h for help, \q to quit.`)

	// Statements are accumulated until a semicolon, so a query can span lines.
	var buffer strings.Builder
	for {
		if buffer.Len() == 0 {
			fmt.Print("quill> ")
		} else {
			fmt.Print("    -> ")
		}
		if !scanner.Scan() {
			fmt.Println()
			return scanner.Err()
		}
		line := scanner.Text()

		// Backslash commands only make sense on a fresh line, not mid-statement.
		if buffer.Len() == 0 && strings.HasPrefix(strings.TrimSpace(line), `\`) {
			if done := runMetaCommand(database, strings.TrimSpace(line)); done {
				return nil
			}
			continue
		}

		buffer.WriteString(line)
		buffer.WriteString("\n")
		if !strings.Contains(line, ";") {
			continue
		}

		statement := buffer.String()
		buffer.Reset()
		if strings.TrimSpace(statement) == "" {
			continue
		}

		start := time.Now()
		results, err := session.ExecuteMany(statement)
		elapsed := time.Since(start)

		for _, res := range results {
			printResult(os.Stdout, res)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			continue
		}
		if len(results) > 0 {
			fmt.Printf("(%s)\n\n", elapsed.Round(time.Microsecond))
		}
	}
}

// runMetaCommand handles the backslash commands, reporting whether to quit.
func runMetaCommand(database *db.DB, line string) bool {
	fields := strings.Fields(line)
	switch fields[0] {
	case `\q`, `\quit`:
		return true

	case `\h`, `\help`:
		fmt.Println(`  \d            list tables
  \d <table>    describe a table
  \stats        buffer pool and storage statistics
  \checkpoint   flush everything and truncate the log
  \q            quit

  SQL: SELECT, INSERT, UPDATE, DELETE, CREATE TABLE, CREATE INDEX,
       DROP TABLE, BEGIN, COMMIT, ROLLBACK, EXPLAIN`)

	case `\d`:
		if len(fields) > 1 {
			describeTable(database, fields[1])
			return false
		}
		tables := database.Catalog().Tables()
		if len(tables) == 0 {
			fmt.Println("no tables")
			return false
		}
		for _, t := range tables {
			fmt.Printf("  %-24s %d columns\n", t.Name, len(t.Columns))
		}

	case `\stats`:
		s := database.Stats()
		fmt.Printf("  pages         %d (%d KB)\n", s.Pages, s.Pages*4)
		fmt.Printf("  tables        %d\n", s.Tables)
		fmt.Printf("  pool hits     %d\n", s.Pager.Hits)
		fmt.Printf("  pool misses   %d\n", s.Pager.Misses)
		fmt.Printf("  disk reads    %d\n", s.Pager.Reads)
		fmt.Printf("  disk writes   %d\n", s.Pager.Writes)
		fmt.Printf("  evictions     %d\n", s.Pager.Evictions)
		fmt.Printf("  wal size      %d bytes\n", s.WALSize)

	case `\checkpoint`:
		if err := database.Checkpoint(); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			return false
		}
		fmt.Println("checkpoint complete")

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q, try \\h\n", fields[0])
	}
	return false
}

func describeTable(database *db.DB, name string) {
	table, ok := database.Catalog().Table(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "table %q does not exist\n", name)
		return
	}
	fmt.Printf("Table %q\n", table.Name)
	fmt.Printf("  %-20s %-10s %s\n", "COLUMN", "TYPE", "MODIFIERS")
	for _, col := range table.Columns {
		var mods []string
		if col.PrimaryKey {
			mods = append(mods, "primary key")
		}
		if col.NotNull && !col.PrimaryKey {
			mods = append(mods, "not null")
		}
		if col.Unique {
			mods = append(mods, "unique")
		}
		fmt.Printf("  %-20s %-10s %s\n", col.Name, col.Type, strings.Join(mods, ", "))
	}
	for _, idx := range table.Indexes {
		var cols []string
		for _, c := range idx.Columns {
			cols = append(cols, table.Columns[c].Name)
		}
		kind := "index"
		if idx.Unique {
			kind = "unique index"
		}
		fmt.Printf("  %s %q on (%s)\n", kind, idx.Name, strings.Join(cols, ", "))
	}
}

// printResult renders a result as an aligned table.
func printResult(out *os.File, res *exec.Result) {
	if len(res.Columns) == 0 {
		if res.Tag != "" {
			fmt.Fprintln(out, res.Tag)
		}
		return
	}

	// Column widths are computed from the data, so the table lines up.
	widths := make([]int, len(res.Columns))
	for i, name := range res.Columns {
		widths[i] = len(name)
	}
	cells := make([][]string, len(res.Rows))
	for i, row := range res.Rows {
		cells[i] = make([]string, len(row))
		for j, v := range row {
			text := v.String()
			if v.IsNull() {
				text = "NULL"
			}
			cells[i][j] = text
			if j < len(widths) && len(text) > widths[j] {
				widths[j] = len(text)
			}
		}
	}

	var header strings.Builder
	var divider strings.Builder
	for i, name := range res.Columns {
		if i > 0 {
			header.WriteString(" | ")
			divider.WriteString("-+-")
		}
		header.WriteString(pad(name, widths[i]))
		divider.WriteString(strings.Repeat("-", widths[i]))
	}
	fmt.Fprintln(out, " "+header.String())
	fmt.Fprintln(out, " "+divider.String())

	for _, row := range cells {
		var line strings.Builder
		for i, cell := range row {
			if i > 0 {
				line.WriteString(" | ")
			}
			width := 0
			if i < len(widths) {
				width = widths[i]
			}
			line.WriteString(pad(cell, width))
		}
		fmt.Fprintln(out, " "+line.String())
	}

	plural := "rows"
	if len(res.Rows) == 1 {
		plural = "row"
	}
	fmt.Fprintf(out, "(%d %s)\n", len(res.Rows), plural)
}

func pad(s string, width int) string {
	if len(s) >= width {
		return s
	}
	return s + strings.Repeat(" ", width-len(s))
}

func parseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q", s)
	}
}
