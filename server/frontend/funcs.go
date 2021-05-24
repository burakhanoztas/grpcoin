// Copyright 2021 Ahmet Alp Balkan
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package frontend

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/grpcoin/grpcoin/server/userdb"
	"github.com/hako/durafmt"
)

var (
	funcs = template.FuncMap{
		"fmtAmount":   fmtAmount,
		"fmtPrice":    fmtPrice,
		"fmtDate":     fmtDate,
		"fmtDuration": fmtDuration,
		"fmtPercent":  fmtPercent,
		"pv":          valuation,
		"isNegative":  isNegative,
		"since":       since,
		"mul":         mul,
		"profilePic":  profilePic,
	}
)

func fmtAmount(a userdb.Amount) string {
	v := fmt.Sprintf("%s.%09d", humanize.Comma(a.Units), a.Nanos)
	vv := strings.TrimRight(v, "0")
	if strings.HasSuffix(vv, ".") {
		vv += "00"
	}
	return vv
}

func fmtPrice(a userdb.Amount) string {
	return fmt.Sprintf("$%s.%02d", humanize.Comma(a.Units), a.Nanos/10000000)
}

func fmtPercent(a userdb.Amount) string {
	nanos := a.Nanos
	if isNegative(a) {
		nanos = -nanos
	}
	v := fmt.Sprintf("%d.%02d%%", a.Units, nanos/10000000)
	if isNegative(a) && v[0] != '-' {
		v = "-" + v
	}
	return v
}

func isNegative(a userdb.Amount) bool {
	return a.Units < 0 || a.Nanos < 0
}

func valuation(p userdb.Portfolio, quotes map[string]userdb.Amount) userdb.Amount {
	total := p.CashUSD.F()
	for curr, amt := range p.Positions {
		// TODO we are not returning an error if quotes don't list the held currency
		total = total.Add(amt.F().Mul(quotes[curr].F()))
	}
	return userdb.ToAmount(total)
}

func mul(a, b userdb.Amount) userdb.Amount {
	return userdb.ToAmount(a.F().Mul(b.F()))
}

func fmtDate(t time.Time) string { return t.Truncate(time.Hour * 24).Format("2 January 2006") }

func since(t time.Time) time.Duration { return time.Since(t) }

func fmtDuration(t time.Duration, maxUnits int) string {
	return durafmt.Parse(t).LimitFirstN(maxUnits).String()
}

func profilePic(profileURL string) string {
	if strings.HasPrefix(profileURL, "https://github.com/") {
		return profileURL + ".png?s=512"
	}
	return ""
}
