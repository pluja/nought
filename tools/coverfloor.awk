# SPDX-License-Identifier: AGPL-3.0-or-later
#
# Checks per-package statement coverage in a Go cover profile against
# floors given as -v floors="store=90 index=90". A block listed by several
# test binaries counts once, covered if any of them covered it.
BEGIN {
	n = split(floors, pairs, " ")
	for (i = 1; i <= n; i++) {
		split(pairs[i], kv, "=")
		floor[kv[1]] = kv[2]
	}
}
NR > 1 {
	split($1, loc, ":")
	pkg = loc[1]
	sub("/[^/]*$", "", pkg)
	sub(".*/", "", pkg)
	if (!(pkg in floor)) next
	if (!($1 in stmts)) { stmts[$1] = $2; owner[$1] = pkg }
	if ($3 > 0) hit[$1] = 1
}
END {
	for (b in stmts) {
		total[owner[b]] += stmts[b]
		if (b in hit) covered[owner[b]] += stmts[b]
	}
	failed = 0
	for (p in floor) {
		pct = total[p] ? 100 * covered[p] / total[p] : 0
		printf "%-10s %5.1f%% (floor %s%%)\n", p, pct, floor[p]
		if (pct < floor[p]) { print "coverage of " p " is below its floor"; failed = 1 }
	}
	exit failed
}
