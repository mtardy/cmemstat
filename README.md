# cmemstat

A utility to retrieve memory stats of a running process from the memory cgroup.

How does that work? The tool starts itself as a child process and `SIGSTOP`
itself. The parent `wait4(2)` for the child state to be `stopped`, then creates
a cgroup using [libcontainer](https://github.com/opencontainers/runc/tree/main/libcontainer)
and puts this init child process inside of it. Then the parent sends `SIGCONT`
to the child that immediately executes the desired command, and finally, the
parent periodically prints the cgroup memory stats. Almost nothing is performed
by the child between `SIGSTOP` and `SIGCONT` so the memory measurement by the
cgroup should be accurately associated to the target command.

## Installation

```shell
go install github.com/mtardy/cmemstat@latest
```

## Usage

```shell
cmemstat [option]... program [programoption]...
```

For example, to retrieve the memory usage of `sleep 3` with the `--debug` option
refreshing the stats every 400ms:

```shell
cmemstat --debug --refresh 400ms sleep 3
```

Or with default options:

```shell
cmemstat sleep 3
```
