// Package version is the one place the release number is written down. It is a constant in the
// source rather than a linker flag because the image is built from a checkout on the box it runs
// on, and a value that has to be passed at build time is a value that will be missing from the
// build somebody does by hand at midnight.
package version

const Current = "0.3.3"
