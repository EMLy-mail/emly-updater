// Code generated from versioninfo.json by "go generate"; DO NOT EDIT.
// Run "go generate ./..." after bumping versioninfo.json.

package ipc

// Version is this build's own semver, stamped into every outgoing
// Envelope's sender_version field and (when self-update is enabled) compared
// against the updater's own update manifest. versioninfo.json is the single
// source of truth; this file is regenerated from it.
const Version = "1.2.0b"
