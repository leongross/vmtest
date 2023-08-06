// Copyright 2018 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package qemu

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"

	"github.com/u-root/gobusybox/src/pkg/golang"
	"github.com/u-root/u-root/pkg/ulog/ulogtest"
	"github.com/u-root/u-root/pkg/uroot"
	"github.com/u-root/u-root/pkg/uroot/initramfs"
	"golang.org/x/exp/slices"
)

func replaceCtl(str []byte) []byte {
	for i, c := range str {
		if c == 9 || c == 10 {
		} else if c < 32 || c == 127 {
			str[i] = '~'
		}
	}
	return str
}

type cmdlineEqualOpt func(*cmdlineEqualOption)

func withArgv0(argv0 string) func(*cmdlineEqualOption) {
	return func(o *cmdlineEqualOption) {
		o.argv0 = argv0
	}
}

func withArg(arg ...string) func(*cmdlineEqualOption) {
	return func(o *cmdlineEqualOption) {
		o.components = append(o.components, arg)
	}
}

type cmdlineEqualOption struct {
	argv0      string
	components [][]string
}

func isCmdlineEqual(got []string, opts ...cmdlineEqualOpt) error {
	var opt cmdlineEqualOption
	for _, o := range opts {
		o(&opt)
	}

	if len(got) == 0 && len(opt.argv0) == 0 && len(opt.components) == 0 {
		return nil
	}
	if len(got) == 0 {
		return fmt.Errorf("empty cmdline")
	}
	if got[0] != opt.argv0 {
		return fmt.Errorf("argv0 does not match: got %v, want %v", got[0], opt.argv0)
	}
	got = got[1:]
	for _, component := range opt.components {
		found := false
		for i := range got {
			if slices.Compare(got[i:i+len(component)], component) == 0 {
				found = true
				got = slices.Delete(got, i, i+len(component))
				break
			}
		}
		if !found {
			return fmt.Errorf("cmdline component %#v not found", component)
		}
	}
	if len(got) > 0 {
		return fmt.Errorf("extraneous cmdline arguments: %#v", got)
	}
	return nil
}

func TestCmdline(t *testing.T) {
	resetVars := []string{
		"VMTEST_QEMU",
		"VMTEST_QEMU_ARCH",
		"VMTEST_KERNEL",
		"VMTEST_INITRAMFS",
	}
	// In case these env vars are actually set by calling env & used below
	// in other tests, save their values, set them to empty for duration of
	// test & restore them after.
	savedEnv := make(map[string]string)
	for _, key := range resetVars {
		savedEnv[key] = os.Getenv(key)
		os.Setenv(key, "")
	}
	t.Cleanup(func() {
		for key, val := range savedEnv {
			os.Setenv(key, val)
		}
	})

	for _, tt := range []struct {
		name string
		o    *Options
		want []cmdlineEqualOpt
		envv map[string]string
		err  error
	}{
		{
			name: "simple",
			o: &Options{
				QEMUPath: "qemu",
				QEMUArch: GuestArchX8664,
				Kernel:   "./foobar",
			},
			want: []cmdlineEqualOpt{
				withArgv0("qemu"),
				withArg("-nographic"),
				withArg("-kernel", "./foobar"),
			},
		},
		{
			name: "kernel-args-fail",
			o: &Options{
				QEMUPath:   "qemu",
				QEMUArch:   GuestArchX8664,
				KernelArgs: "printk=ttyS0",
			},
			err: ErrKernelRequiredForArgs,
		},
		{
			name: "device-kernel-args-fail",
			o: &Options{
				QEMUPath: "qemu",
				QEMUArch: GuestArchX8664,
				Devices:  []Device{ArbitraryKernelArgs{"earlyprintk=ttyS0"}},
			},
			err: ErrKernelRequiredForArgs,
		},
		{
			name: "kernel-args-initrd-with-precedence-over-env",
			o: &Options{
				QEMUPath:   "qemu",
				QEMUArch:   GuestArchX8664,
				Kernel:     "./foobar",
				Initramfs:  "./initrd",
				KernelArgs: "printk=ttyS0",
				Devices:    []Device{ArbitraryKernelArgs{"earlyprintk=ttyS0"}},
			},
			envv: map[string]string{
				"VMTEST_QEMU":      "qemu-system-x86_64 -enable-kvm -m 1G",
				"VMTEST_QEMU_ARCH": "i386",
				"VMTEST_KERNEL":    "./baz",
				"VMTEST_INITRAMFS": "./init.cpio",
			},
			want: []cmdlineEqualOpt{
				withArgv0("qemu"),
				withArg("-nographic"),
				withArg("-kernel", "./foobar"),
				withArg("-initrd", "./initrd"),
				withArg("-append", "printk=ttyS0 earlyprintk=ttyS0"),
			},
		},
		{
			name: "device-kernel-args",
			o: &Options{
				QEMUPath: "qemu",
				QEMUArch: GuestArchX8664,
				Kernel:   "./foobar",
				Devices:  []Device{ArbitraryKernelArgs{"earlyprintk=ttyS0"}},
			},
			want: []cmdlineEqualOpt{
				withArgv0("qemu"),
				withArg("-nographic"),
				withArg("-kernel", "./foobar"),
				withArg("-append", "earlyprintk=ttyS0"),
			},
		},
		{
			name: "id-allocator",
			o: &Options{
				QEMUPath: "qemu",
				QEMUArch: GuestArchX8664,
				Kernel:   "./foobar",
				Devices: []Device{
					IDEBlockDevice{"./disk1"},
					IDEBlockDevice{"./disk2"},
				},
			},
			want: []cmdlineEqualOpt{
				withArgv0("qemu"),
				withArg("-nographic"),
				withArg("-kernel", "./foobar"),
				withArg("-drive", "file=./disk1,if=none,id=drive0",
					"-device", "ich9-ahci,id=ahci0",
					"-device", "ide-hd,drive=drive0,bus=ahci0.0"),
				withArg("-drive", "file=./disk2,if=none,id=drive1",
					"-device", "ich9-ahci,id=ahci1",
					"-device", "ide-hd,drive=drive1,bus=ahci1.0"),
			},
		},
		{
			name: "env-config",
			o:    &Options{},
			envv: map[string]string{
				"VMTEST_QEMU":      "qemu-system-x86_64 -enable-kvm -m 1G",
				"VMTEST_QEMU_ARCH": "x86_64",
				"VMTEST_KERNEL":    "./foobar",
				"VMTEST_INITRAMFS": "./init.cpio",
			},
			want: []cmdlineEqualOpt{
				withArgv0("qemu-system-x86_64"),
				withArg("-nographic"),
				withArg("-enable-kvm", "-m", "1G"),
				withArg("-initrd", "./init.cpio"),
				withArg("-kernel", "./foobar"),
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for key, val := range tt.envv {
				os.Setenv(key, val)
			}
			t.Cleanup(func() {
				for key := range tt.envv {
					os.Setenv(key, "")
				}
			})
			got, err := tt.o.Cmdline()
			if !errors.Is(err, tt.err) {
				t.Errorf("Cmdline = %v, want %v", err, tt.err)
			}

			t.Logf("Got cmdline: %v", got)
			if err := isCmdlineEqual(got, tt.want...); err != nil {
				t.Errorf("Cmdline = %v", err)
			}
		})
	}
}

// goarchToQEMUArch maps GOARCH to QEMU arch values.
var goarchToQEMUArch = map[string]GuestArch{
	"386":   GuestArchI386,
	"amd64": GuestArchX8664,
	"arm":   GuestArchArm,
	"arm64": GuestArchAarch64,
}

func guestGOARCH() string {
	if env := os.Getenv("VMTEST_GOARCH"); env != "" {
		return env
	}
	return runtime.GOARCH
}

func TestStartVM(t *testing.T) {
	tmp := t.TempDir()
	logger := &ulogtest.Logger{TB: t}
	initrdPath := filepath.Join(tmp, "initramfs.cpio")
	initrdWriter, err := initramfs.CPIO.OpenWriter(logger, initrdPath)
	if err != nil {
		t.Fatalf("Failed to create initramfs writer: %v", err)
	}

	env := golang.Default()
	env.CgoEnabled = false
	env.GOARCH = guestGOARCH()

	uopts := uroot.Opts{
		Env:        &env,
		InitCmd:    "init",
		UinitCmd:   "qemutest1",
		OutputFile: initrdWriter,
		TempDir:    tmp,
	}
	uopts.AddBusyBoxCommands(
		"github.com/u-root/u-root/cmds/core/init",
		"github.com/hugelgupf/vmtest/qemu/qemutest1",
	)
	if err := uroot.CreateInitramfs(logger, uopts); err != nil {
		t.Fatalf("error creating initramfs: %v", err)
	}

	r, w := io.Pipe()
	opts := &Options{
		// Using VMTEST_KERNEL && VMTEST_QEMU.
		QEMUArch:     goarchToQEMUArch[guestGOARCH()],
		Initramfs:    initrdPath,
		SerialOutput: w,
	}
	if arch, err := opts.Arch(); err != nil {
		t.Fatal(err)
	} else if arch == "arm" {
		opts.KernelArgs = "console=ttyAMA0"
	} else if arch == "x86_64" {
		opts.KernelArgs = "console=ttyS0 earlyprintk=ttyS0"
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		s := bufio.NewScanner(r)
		for s.Scan() {
			t.Logf("vm: %s", replaceCtl(s.Bytes()))
		}
		if err := s.Err(); err != nil {
			t.Errorf("Error reading serial from VM: %v", err)
		}
	}()

	vm, err := opts.Start()
	if err != nil {
		t.Fatalf("Failed to start VM: %v", err)
	}
	t.Logf("cmdline: %#v", vm.CmdlineQuoted())

	if _, err := vm.Console.ExpectString("I AM HERE"); err != nil {
		t.Errorf("Error expecting I AM HERE: %v", err)
	}

	if err := vm.Wait(); err != nil {
		t.Fatalf("Error waiting for VM to exit: %v", err)
	}
	wg.Wait()
}