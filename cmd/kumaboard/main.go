// Command kumaboard is the control plane server.
package main

import (
	"fmt"
	"os"

	"github.com/jhyoong/KumaBoard/internal/buildinfo"
	"github.com/jhyoong/KumaBoard/proto"
)

func usage() {
	fmt.Fprintln(os.Stderr, `usage: kumaboard <command> [flags]

commands:
  serve          run the server (flags: -config PATH)
  device add     register a device and print its token once
  passwd         create the operator account or reset its password (flags: -config PATH)
  cert reissue   issue a new server certificate from the existing CA (flags: -config PATH)
  sign           sign release artifacts (flags: --version, -config PATH)
  release ingest ingest signed release into database (flags: --version, -config PATH)
  version        print version and protocol version`)
	os.Exit(2)
}

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = runServe(os.Args[2:])
	case "device":
		if len(os.Args) < 3 || os.Args[2] != "add" {
			usage()
		}
		err = runDeviceAdd(os.Args[3:])
	case "passwd":
		err = runPasswd(os.Args[2:])
	case "cert":
		if len(os.Args) < 3 || os.Args[2] != "reissue" {
			usage()
		}
		err = runCertReissue(os.Args[3:])
	case "sign":
		err = runSign(os.Args[2:])
	case "release":
		if len(os.Args) < 3 || os.Args[2] != "ingest" {
			usage()
		}
		err = runReleaseIngest(os.Args[3:])
	case "version":
		fmt.Printf("kumaboard %s protocol %d\n", buildinfo.Version, proto.Version)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
