// Copyright 2023 The Chromium Authors
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

// Package recall is recall subcommand to recall action by the digest
// and execute cmd with remote exec API.
package recall

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	rpb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	log "github.com/golang/glog"
	"github.com/google/uuid"
	"github.com/maruel/subcommands"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	tspb "google.golang.org/protobuf/types/known/timestamppb"

	"go.chromium.org/luci/auth"
	"go.chromium.org/luci/common/cli"
	"go.chromium.org/luci/common/system/signals"

	"infra/build/siso/auth/cred"
	"infra/build/siso/reapi"
	"infra/build/siso/reapi/digest"
	"infra/build/siso/reapi/merkletree/exporter"
	"infra/build/siso/reapi/merkletree/importer"
)

const usage = `recall action by digest, or remote exec call to run.

To recall action identified by <digest> in <dir>.

 $ siso recall -project <project> -reapi_instance <instance> \
	    <dir> <digest>

In <dir>, you'll get "action.txt", "command.txt" and "root/".

You may modify *.txt or files in root/.
From in the <dir>,
 - To issue remote exec call to run the command.

 $ siso recall -project <project> -reapi_instance <instance> \
	    <dir>

 - To re-run the command on a remote worker without fetching cache.

 $ siso recall -project <project> -reapi_instance <instance> -re_cache_enable_read=false \
	    <dir>

 - To use local docker to run the command.

 $ siso recall [-local] <dir>
`

// Cmd returns the Command for the `recall` subcommand provided by this package.
func Cmd(authOpts cred.Options) *subcommands.Command {
	return &subcommands.Command{
		UsageLine: "recall <args>....",
		ShortDesc: "recall action",
		LongDesc:  usage,
		CommandRun: func() subcommands.CommandRun {
			c := &run{
				authOpts: authOpts,
			}
			c.init()
			return c
		},
	}
}

type run struct {
	subcommands.CommandRunBase

	authOpts          cred.Options
	projectID         string
	reopt             *reapi.Option
	executeRequestStr string
	reCacheEnableRead bool
	cpuLimit          string
	memLimit          string
	hddMode           bool
	local             bool
	stats             bool
}

func (c *run) init() {
	c.Flags.StringVar(&c.projectID, "project", os.Getenv("SISO_PROJECT"), "cloud project ID. can be set by $SISO_PROJECT")
	c.Flags.BoolVar(&c.reCacheEnableRead, "re_cache_enable_read", true, "remote exec cache enable read")
	c.Flags.StringVar(&c.cpuLimit, "cpus", "", "how much of the available CPU resources the action can use (e.g. '1.5' for at most one and a half of the CPUs)")
	c.Flags.StringVar(&c.memLimit, "memory", "", "the maximum amount of memory the action can use (e.g. 512m or 2g)")
	c.Flags.BoolVar(&c.hddMode, "hdd", false, "run the action with slowed down I/O that resembles a hard-disk with 12MB/s throughput, 75 read IOPS and 150 write IOPS")
	c.Flags.BoolVar(&c.local, "local", false, "force running the action locally using Docker, even if REAPI is configured")
	c.Flags.BoolVar(&c.stats, "stats", false, "run the command under /usr/bin/time and print detailed resource stats after execution (note: this may fail if the container glibc is incompatible with the host)")
	c.reopt = new(reapi.Option)
	envs := map[string]string{
		"SISO_REAPI_ADDRESS":  os.Getenv("SISO_REAPI_ADDRESS"),
		"SISO_REAPI_INSTANCE": os.Getenv("SISO_REAPI_INSTANCE"),
	}
	c.reopt.RegisterFlags(&c.Flags, envs)
	c.Flags.StringVar(&c.executeRequestStr, "execute_request", "", "execute request proto")
}

func (c *run) Run(a subcommands.Application, args []string, env subcommands.Env) int {
	ctx := cli.GetContext(a, c, env)
	err := c.run(ctx)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrLoginRequired):
			fmt.Fprintf(os.Stderr, "need to login: run `siso login`\n")
		case errors.Is(err, flag.ErrHelp):
			fmt.Fprintf(os.Stderr, "%s\n", usage)
		default:
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		}
		return 1
	}
	return 0
}

func (c *run) run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer signals.HandleInterrupt(cancel)()

	projectID := c.reopt.UpdateProjectID(c.projectID)
	var credential cred.Cred
	var err error
	if projectID != "" {
		credential, err = cred.New(ctx, c.authOpts)
		if err != nil {
			return err
		}
	}
	dir := "."
	if c.Flags.NArg() >= 1 {
		dir = c.Flags.Arg(0)
	}
	if c.Flags.NArg() <= 1 {
		err = os.Chdir(dir)
		if err != nil {
			return err
		}
		executeReq := &rpb.ExecuteRequest{}
		err = prototext.Unmarshal([]byte(c.executeRequestStr), executeReq)
		if err != nil {
			return err
		}
		executeReq.SkipCacheLookup = !c.reCacheEnableRead
		return c.call(ctx, *c.reopt, credential, executeReq)
	}
	client, err := reapi.New(ctx, credential, *c.reopt)
	if err != nil {
		return err
	}
	defer client.Close()
	err = os.MkdirAll(dir, 0755)
	if err != nil {
		return err
	}
	err = os.Chdir(dir)
	if err != nil {
		return err
	}

	actionDigest, err := digest.Parse(c.Flags.Arg(1))
	if err != nil {
		return err
	}
	action := &rpb.Action{}
	err = client.Proto(ctx, actionDigest, action)
	if err != nil {
		return err
	}
	err = os.WriteFile("action.txt", []byte(prototext.Format(action)), 0644)
	if err != nil {
		return err
	}

	command := &rpb.Command{}
	err = client.Proto(ctx, digest.FromProto(action.CommandDigest), command)
	if err != nil {
		return err
	}
	err = os.WriteFile("command.txt", []byte(prototext.Format(command)), 0644)
	if err != nil {
		return err
	}

	e := exporter.New(client)
	err = e.Export(ctx, "root", digest.FromProto(action.InputRootDigest), nil)
	if err != nil {
		return err
	}

	result, err := client.GetActionResult(ctx, actionDigest)
	switch status.Code(err) {
	case codes.OK:
		err = os.WriteFile("result.txt", []byte(prototext.Format(result)), 0644)
		if err != nil {
			return err
		}
	case codes.NotFound:
	default:
		return fmt.Errorf("failed to get action result of %s: %w", actionDigest, err)
	}
	return nil

}

func (c *run) call(ctx context.Context, reopt reapi.Option, credential cred.Cred, executeReq *rpb.ExecuteRequest) error {
	_, err := os.Stat("command.txt")
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return flag.ErrHelp
	}

	if c.local || !reopt.IsValid() {
		return c.callLocal(ctx)
	}
	client, err := reapi.New(ctx, credential, reopt)
	if err != nil {
		return err
	}
	defer client.Close()
	toolInvocationID := uuid.New().String()
	actionID := uuid.New().String()
	ctx = reapi.NewContext(ctx, &rpb.RequestMetadata{
		ActionId:         actionID,
		ToolInvocationId: toolInvocationID,
	})
	fmt.Printf("Time: %s\n", time.Now().Format(time.RFC3339))
	fmt.Printf("ToolInvocationID: %s\n", toolInvocationID)
	fmt.Printf("ActionID: %s\n", actionID)

	ds := digest.NewStore()
	var inputRootDigest digest.Digest
	_, err = os.Stat("root")
	if err == nil {
		fmt.Printf("input root from root/: ")
		var im importer.Importer
		inputRootDigest, err = im.Import(ctx, "root", ds)
		if err != nil {
			return err
		}
		log.Infof("inputRoot: %s", inputRootDigest)
		fmt.Printf("%s\n", inputRootDigest)
	} else {
		log.Infof("no root directory: %v", err)
	}

	var commandDigest digest.Digest
	var platform *rpb.Platform
	_, err = os.Stat("command.txt")
	if err == nil {
		fmt.Printf("command from command.txt: ")
		command := &rpb.Command{}
		err = loadTextProto("command.txt", command)
		if err != nil {
			return err
		}
		platform = command.Platform
		data, err := digest.FromProtoMessage(command)
		if err != nil {
			return err
		}
		commandDigest = data.Digest()
		log.Infof("command: %s", commandDigest)
		fmt.Printf("%s\n", commandDigest)
		ds.Set(data)
	} else {
		log.Infof("failed to access command.txt: %v", err)
	}

	action := &rpb.Action{}
	err = loadTextProto("action.txt", action)
	if err != nil {
		return err
	}
	if !commandDigest.IsZero() {
		action.CommandDigest = commandDigest.Proto()
		if platform != nil && action.Platform != nil && !proto.Equal(platform, action.Platform) {
			return fmt.Errorf("platform mismatch: command=%s action=%s", platform, action.Platform)
		}
	}
	if !inputRootDigest.IsZero() {
		action.InputRootDigest = inputRootDigest.Proto()
	}
	log.Infof("action: %s", action)
	data, err := digest.FromProtoMessage(action)
	if err != nil {
		return err
	}
	actionDigest := data.Digest()
	log.Infof("action: %s", actionDigest)
	fmt.Printf("Action: %s\n", actionDigest)
	ds.Set(data)

	n, err := client.UploadAll(ctx, ds)
	if err != nil {
		return err
	}
	log.Infof("upload %d/%d", n, len(ds.List()))
	executeReq.ActionDigest = actionDigest.Proto()
	log.Infof("execute req: %s", executeReq)
	opName, resp, err := client.ExecuteAndWait(ctx, executeReq)
	log.Infof("operation: %s", opName)
	log.Infof("response: %s", resp)
	if err != nil {
		log.Errorf("err: %v", err)
	}
	result := resp.GetResult()
	fmt.Printf("exit=%d\n", result.GetExitCode())
	fmt.Printf("cache=%t\n", resp.GetCachedResult())
	if len(result.GetOutputFiles()) > 0 {
		fmt.Printf("output_files\n")
		for _, f := range result.GetOutputFiles() {
			fmt.Printf("%s\n", f.GetPath())
			fmt.Printf("  - %v\n", f.GetDigest())
		}
	}
	if len(result.GetOutputDirectories()) > 0 {
		fmt.Printf("output_dirs\n")
		for _, d := range result.GetOutputDirectories() {
			fmt.Printf(" %s\n", d.GetPath())
			fmt.Printf("  - %v\n", d.GetTreeDigest())
		}
	}
	if result.GetStdoutDigest() != nil {
		fmt.Printf("stdout: %v\n", result.GetStdoutDigest())
	}
	if result.GetStderrDigest() != nil {
		fmt.Printf("stderr: %v\n", result.GetStderrDigest())
	}
	md := result.GetExecutionMetadata()
	queueTime := timestampSub(md.GetWorkerStartTimestamp(), md.GetQueuedTimestamp())
	workerTime := timestampSub(md.GetWorkerCompletedTimestamp(), md.GetWorkerStartTimestamp())
	inputTime := timestampSub(md.GetInputFetchCompletedTimestamp(), md.GetInputFetchStartTimestamp())
	execTime := timestampSub(md.GetExecutionCompletedTimestamp(), md.GetExecutionStartTimestamp())
	outputTime := timestampSub(md.GetOutputUploadCompletedTimestamp(), md.GetOutputUploadStartTimestamp())
	fmt.Printf("execution metadata: %s\n", md.GetWorker())
	fmt.Printf("  queue: %s\n", queueTime)
	fmt.Printf("  worker: %s\n", workerTime)
	fmt.Printf("    input: %s\n", inputTime)
	fmt.Printf("    exec: %s\n", execTime)
	fmt.Printf("    output: %s\n", outputTime)
	return err
}

func timestampSub(t1, t2 *tspb.Timestamp) time.Duration {
	time1 := t1.AsTime()
	time2 := t2.AsTime()
	return time1.Sub(time2)
}

func loadTextProto(fname string, p proto.Message) error {
	b, err := os.ReadFile(fname)
	if err != nil {
		return err
	}
	err = prototext.Unmarshal(b, p)
	if err != nil {
		return fmt.Errorf("failed to unmarshal %s: %v", fname, err)
	}
	return nil
}

func (c *run) callLocal(ctx context.Context) error {
	command := &rpb.Command{}
	err := loadTextProto("command.txt", command)
	if err != nil {
		return err
	}
	_, err = os.Stat("passwd")
	if err != nil {
		// generate passwd file that contains user's passwd entry
		// otherwise, docker run will fail because it can't find
		// user in /etc/passwd.
		err = genPasswd(ctx, "passwd")
		if err != nil {
			return err
		}
	}
	passwd, err := filepath.Abs("passwd")
	if err != nil {
		return err
	}
	root, err := filepath.Abs("root")
	if err != nil {
		return err
	}
	wd := filepath.Join(root, command.WorkingDirectory)
	for _, output := range command.OutputFiles {
		odir := filepath.Join(wd, filepath.Dir(output))
		err = os.MkdirAll(odir, 0755)
		if err != nil {
			return err
		}
		// may need to be writable for nobody?
		err = os.Chmod(odir, 0777)
		if err != nil {
			return err
		}
	}
	p := command.Platform
	inputRootDir := "/mnt"
	if v := platformProperty(p, "InputRootAbsolutePath"); v != "" {
		inputRootDir = v
	}
	cmdline := []string{"docker", "run", "--rm",
		"-v", passwd + ":/etc/passwd",
		"-v", root + ":" + inputRootDir,
		"-w", filepath.Join(inputRootDir, command.WorkingDirectory),
	}
	for _, e := range command.EnvironmentVariables {
		cmdline = append(cmdline, "-e", fmt.Sprintf("%s=%s", e.Name, e.Value))
	}
	if v := platformProperty(p, "dockerPrivileged"); v == "true" {
		cmdline = append(cmdline, "--privileged")
	}
	if v := platformProperty(p, "dockerRunAsRoot"); v != "true" {
		uid, err := uidInPasswd("passwd")
		if err != nil {
			return err
		}
		cmdline = append(cmdline, "--user", uid)
	}

	// Limit CPU resources if requested.
	if c.cpuLimit != "" {
		cmdline = append(cmdline, "--cpus="+c.cpuLimit)
	}

	// Limit memory if requested.
	if c.memLimit != "" {
		cmdline = append(cmdline, "--memory="+c.memLimit)
	}

	// Limit I/O performance if requested.
	if c.hddMode {
		// Find physical device for the working directory.
		deviceBytes, err := exec.CommandContext(ctx, "findmnt", "-no", "source", "-T", root).Output()
		if err != nil {
			return err
		}
		device := strings.TrimSpace(string(deviceBytes))

		// Simulate the performance of a 100GB pd-standard disk.
		// https://cloud.google.com/compute/docs/disks/performance#zonal-persistent-disks
		cmdline = append(cmdline, "--device-read-iops", device+":75")
		cmdline = append(cmdline, "--device-write-iops", device+":150")
		cmdline = append(cmdline, "--device-read-bps", device+":12mb")
		cmdline = append(cmdline, "--device-write-bps", device+":12mb")
	}

	// We cannot assume that the container ships a copy of /usr/bin/time, so let's just mount
	// the one from the current system into the container.
	if c.stats {
		cmdline = append(cmdline, "-v", "/usr/bin/time:/usr/bin/time")
	}

	cmdline = append(cmdline, strings.TrimPrefix(platformProperty(p, "container-image"), "docker://"))

	if c.stats {
		cmdline = append(cmdline, "/usr/bin/time", "-v")
	}

	cmdline = append(cmdline, command.Arguments...)
	fmt.Println(cmdline)
	cmd := exec.CommandContext(ctx, cmdline[0], cmdline[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func genPasswd(ctx context.Context, fname string) error {
	cmd := exec.CommandContext(ctx, "getent", "passwd", os.ExpandEnv("${USER}"))
	buf, err := cmd.Output()
	if err != nil {
		return err
	}
	cmd = exec.CommandContext(ctx, "getent", "passwd", "nobody")
	buf2, err := cmd.Output()
	if err != nil {
		return err
	}
	buf = append(buf, buf2...)
	return os.WriteFile(fname, buf, 0644)
}

func uidInPasswd(fname string) (string, error) {
	buf, err := os.ReadFile(fname)
	if err != nil {
		return "", err
	}
	fields := strings.Split(string(buf), ":")
	if len(fields) < 3 {
		return "", fmt.Errorf("wrong format of passwd %q", buf)
	}
	return fields[2], nil
}

func platformProperty(p *rpb.Platform, key string) string {
	for _, pp := range p.Properties {
		if pp.Name == key {
			return pp.Value
		}
	}
	return ""
}
