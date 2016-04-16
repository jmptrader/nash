# cnt

[![Build Status](https://travis-ci.org/tiago4orion/cnt.svg?branch=master)](https://travis-ci.org/tiago4orion/cnt) [![codecov.io](https://codecov.io/github/tiago4orion/cnt/coverage.svg?branch=master)](https://codecov.io/github/tiago4orion/cnt?branch=master)

Cnt is a simple shell for sane containerized deploys. If you think
every serious developer must have the power of namespace at their
fingers, then `cnt` is for you.

No, it's not POSIX. Every harmful features of mainstream shells was
left behind. Cnt is inspired by Plan 9 `rc` shell, but very different
syntax and purpose.

# Concept

Nowadays everyone agrees that a sane deploy requires linux containers,
but why this kind of tools (docker, rkt, etc) and libraries (lxc,
libcontainer, etc) are so bloated and magical?

Cnt is only a simple shell plus a keyword called `rfork`. Rfork try to
mimic what Plan9 `rfork` does, but with linux limitations in mind.

# Show time!

Go ahead:

```
go get github.com/tiago4orion/cnt/cmd/cnt
# Make sure GOPATH/bin is in yout PATH
cnt
cnt> echo "hello world"
hello world
```

Make sure you have USER namespaces enabled in your kernel:
```bash
zgrep CONFIG_USER_NS /proc/config.gz
CONFIG_USER_NS=y
```

If it's not enabled you will need root privileges to execute every example below...

Creating a new process in a new USER namespace (u):

```
cnt> rfork u {
     id
}
uid=0(root) gid=0(root) groups=0(root),65534
```
Yes, Linux supports creation of containers by unprivileged users. Tell
this to the customer success of your container-infrastructure-vendor.

You can verify that other types of namespace still requires root
capabilities, see for PID namespaces (p).

```
cnt> rfork p {
    id
}
ERROR: fork/exec ./cnt: operation not permitted
```

The same happens for mount (m), ipc (i) and uts (s) if used without
user namespace (u) flag.

The `c` flag stands for "container" and is an alias for upmnis (all
types of namespaces).  If you want a shell inside the container:

```
cnt> rfork c {
    bash
}
[root@stay-away cnt]# id
uid=0(root) gid=0(root) groups=0(root),65534
[root@stay-away cnt]# mount -t proc proc /proc
[root@stay-away cnt]#
[root@stay-away cnt]# ps aux
USER       PID %CPU %MEM    VSZ   RSS TTY      STAT START   TIME COMMAND
root         1  0.0  0.0  34648  2748 pts/4    Sl   17:32   0:00 -rcd- -addr /tmp/cnt.qNQa.sock
root         5  0.0  0.0  16028  3840 pts/4    S    17:32   0:00 /usr/bin/bash
root        23  0.0  0.0  34436  3056 pts/4    R+   17:34   0:00 ps aux
```

Everything except the `rfork` is like a dumb shell. Rfork will spawn a
new process with the namespace flags and executes the commands inside
the block on this namespace. It has the form:

```
rfork <flags> {
    <comands to run inside the container>
}
```

# OK, but what my deploy will look like?

Take a look in the script below:

```bash
#!/usr/bin/env cnt

-rm -rf rootfs

echo "Executing container"

# Forking the container with all namespaces except network
rfork upmis {
    mount -t proc proc /proc
    mkdir -p rootfs
    mount -t tmpfs -o size=1G tmpfs rootfs

    cd rootfs

    wget "https://busybox.net/downloads/binaries/latest/busybox-x86_64" -O busybox
    chmod +x busybox

    mkdir bin

    ./busybox --install ./bin

    mkdir -p proc
    mkdir -p dev
    mount -t proc proc proc
    mount -t tmpfs tmpfs dev

    cp ../my-service .
    chroot . /my-service
}
```

Execute with:

```bash

./cnt -file example.cnt
--2016-04-15 17:54:02--  https://busybox.net/downloads/binaries/latest/busybox-x86_64
Resolving busybox.net (busybox.net)... 140.211.167.224
Connecting to busybox.net (busybox.net)|140.211.167.224|:443... connected.
HTTP request sent, awaiting response... 200 OK
Length: 973200 (950K)
Saving to: ‘busybox’

busybox                 100%[===============================>] 950.39K  21.1KB/s    in 43s

2016-04-15 17:54:46 (22.1 KB/s) - ‘busybox’ saved [973200/973200]

Hello World service
```

Change the last line of chroot to invoke /bin/sh if you want a shell
inside the busybox.

I know, I know, lots of questions in how to handle the hard parts of
deploy. My answer is: Coming soon.


# Want to contribute?

Open issues and PR :)
The project is in an early stage, be patient because things can change
a lot in the future.
