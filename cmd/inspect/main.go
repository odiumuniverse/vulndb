// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command inspect provides insights into the current contents of vulndb.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/exp/maps"
	"golang.org/x/vulndb/internal/ghsarepo"
	"golang.org/x/vulndb/internal/report"
	"golang.org/x/vulndb/internal/stdlib"
	"golang.org/x/vulndb/pkg/genericosv"
)

var (
	localGHSA = flag.String("local-ghsa", "", "path to local GHSA repo, instead of cloning remote")
	detail    = flag.Bool("detail", false, "if true, print more details on GHSAs not yet in vulndb")
)

func main() {
	start := time.Now()
	flag.Parse()

	ctx := context.Background()
	rc, err := report.NewLocalClient(ctx, ".")
	if err != nil {
		log.Fatal(err)
	}

	var gc *ghsarepo.Client
	if *localGHSA != "" {
		gc, err = ghsarepo.NewLocalClient(ctx, *localGHSA)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		log.Println("cloning remote GHSA repo (use -local-ghsa to speed this up)...")
		gc, err = ghsarepo.NewDefaultClient()
		if err != nil {
			log.Fatal(err)
		}
	}

	overall, byYear := summarize(gc.List(), rc.List())

	display(overall, byYear)
	if *detail {
		fmt.Println("\n=== GHSAs not yet in vulndb ===")
		displayGHSAs(overall.ghsasNotInVDB)
	}

	fmt.Printf("\n%s\n", time.Since(start).Truncate(time.Millisecond))
}

func display(overall *summary, byYear map[int]*summary) {
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	years := maps.Keys(byYear)
	sort.Sort(sort.Reverse(sort.IntSlice(years))) // sort descending

	headings := func() {
		fmt.Fprintf(tw, "\t%s", "Total")
		for _, year := range years {
			fmt.Fprintf(tw, "\t%4d", year)
		}
		fmt.Fprintf(tw, "\n")
		fmt.Fprintf(tw, "\t%s", "-----")
		for range years {
			fmt.Fprintf(tw, "\t%4s", "----")
		}
		fmt.Fprintf(tw, "\n")
	}
	data := func(desc string, indent int, getData func(s *summary) int) {
		var indentS string
		if indent > 0 {
			indentS = strings.Repeat("--", indent) + " "
		}
		fmt.Fprintf(tw, "%s%s\t%4d", indentS, desc, getData(overall))
		for _, year := range years {
			fmt.Fprintf(tw, "\t%4d", getData(byYear[year]))
		}
		fmt.Fprintf(tw, "\n")
	}
	newline := func() {
		fmt.Fprintf(tw, "%s\n", strings.Repeat("\t", len(years))) // preserve tab formatting
	}

	// Summary of Go reports by year.
	headings()
	data("Go reports", 0, func(s *summary) int { return s.reports })
	data("Regular reports", 1, func(s *summary) int { return s.regular })
	for _, rs := range []report.ReviewStatus{report.Reviewed, report.Unreviewed} {
		data(rs.String(), 2, func(s *summary) int { return s.regularByReview[rs] })
	}
	data("Withdrawn reports", 1, func(s *summary) int { return s.withdrawn })
	data("Excluded reports", 1, func(s *summary) int { return s.excluded })
	for _, er := range report.ExcludedTypes {
		data(string(er), 2, func(s *summary) int { return s.excludedByType[er] })
	}
	data("Reports with no GHSA (+)", 1, func(s *summary) int { return s.noGHSA })
	data("Stdlib, toolchain and x/ reports", 1, func(s *summary) int { return s.firstParty })

	// Summary of GHSAs by year.
	newline()
	headings()
	data("GHSAs affecting Go", 0, func(s *summary) int { return s.ghsas })
	data("GHSAs not yet in vulndb (++)", 1, func(s *summary) int { return len(s.ghsasNotInVDB) })

	// Additional context.
	newline()
	fmt.Fprintln(tw, "(+) \"Go reports with no GHSA\" are published third-party Go reports\nwith no corresponding GHSA. (This isn't a problem; it's informational only.)")
	fmt.Fprintln(tw, "(++) \"GHSAs not yet in vulndb\" are published GHSAs with no corresponding\nGo report. There may already be an open issue on the tracker for these.")

	tw.Flush()
}

func displayGHSAs(ghsas []string) {
	for i, g := range ghsas {
		fmt.Println()
		fmt.Printf("%d) %s\n", i+1, g)
		fmt.Printf("https://github.com/advisories/%s\n", g)
		fmt.Printf("search issue tracker: https://github.com/golang/vulndb/issues?q=is%%3Aissue+%s\n", g)
	}
}

type summary struct {
	reports, regular, withdrawn, excluded, noGHSA, firstParty int
	ghsas                                                     int
	ghsasNotInVDB                                             []string
	excludedByType                                            map[report.ExcludedType]int
	regularByReview                                           map[report.ReviewStatus]int
}

func newSummary() *summary {
	return &summary{
		excludedByType:  make(map[report.ExcludedType]int),
		regularByReview: make(map[report.ReviewStatus]int),
	}
}

func summarize(ghsas []*genericosv.Entry, reports []*report.Report) (*summary, map[int]*summary) {
	overall := newSummary()
	byYear := make(map[int]*summary)

	ghsasWithReport := make(map[string]bool)
	for _, r := range reports {
		year, err := strconv.Atoi(strings.Split(r.ID, "-")[1])
		if err != nil {
			panic(err)
		}
		if _, ok := byYear[year]; !ok {
			byYear[year] = newSummary()
		}
		yearSummary := byYear[year]

		overall.reports++
		yearSummary.reports++

		if isFirstParty(r) {
			overall.firstParty++
			yearSummary.firstParty++
		}

		if r.IsExcluded() {
			overall.excluded++
			overall.excludedByType[r.Excluded]++

			yearSummary.excluded++
			yearSummary.excludedByType[r.Excluded]++
		} else if r.Withdrawn != nil {
			overall.withdrawn++
			yearSummary.withdrawn++
		} else {
			overall.regular++
			yearSummary.regular++

			overall.regularByReview[r.ReviewStatus]++
			yearSummary.regularByReview[r.ReviewStatus]++
		}

		if len(r.GHSAs) == 0 && r.CVEMetadata == nil {
			overall.noGHSA++
			yearSummary.noGHSA++
		}
		for _, ghsa := range r.GHSAs {
			ghsasWithReport[ghsa] = true
		}
	}

	for _, ghsa := range ghsas {
		year := ghsa.Published.Year()
		if _, ok := byYear[year]; !ok {
			byYear[year] = newSummary()
		}
		yearSummary := byYear[year]
		overall.ghsas++
		yearSummary.ghsas++
		if _, ok := ghsasWithReport[ghsa.ID]; !ok {
			overall.ghsasNotInVDB = append(overall.ghsasNotInVDB, ghsa.ID)
			yearSummary.ghsasNotInVDB = append(yearSummary.ghsasNotInVDB, ghsa.ID)
		}
	}

	return overall, byYear
}

func isFirstParty(r *report.Report) bool {
	for _, m := range r.Modules {
		if stdlib.IsStdModule(m.Module) || stdlib.IsCmdModule(m.Module) || stdlib.IsXModule(m.Module) {
			return true
		}
	}
	return false
}
