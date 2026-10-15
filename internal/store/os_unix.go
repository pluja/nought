// SPDX-License-Identifier: AGPL-3.0-or-later

//go:build !windows

package store

import "os"

// infoSep separates the unique name from the Maildir info ("2,flags").
const infoSep = ":"

func syncDir(d *os.File) error { return d.Sync() }
