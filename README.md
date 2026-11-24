# diskdust

Find the largest directories on your disk that haven't been accessed recently.

Install:

```bash
go install github.com/tsaarni/diskdust@latest
```

or run without installing:

```bash
go run github.com/tsaarni/diskdust@latest
```

## Usage

```bash
diskdust [path] [flags]
```

Path defaults to `$HOME`.

```bash
diskdust                                # biggest directories and files
diskdust --sort dust                    # rank by size and age combined
diskdust --not-accessed 365             # only entries not accessed in 365 days
diskdust --sort accessed                # sort by least recently accessed
diskdust --exclude Downloads/archive    # skip matching paths during the walk
diskdust ~/.cache --min-size 100MB      # focus on a specific directory
```

| Flag | Default | Description |
|------|---------|-------------|
| `--not-accessed` | 0 | Only show entries not accessed in this many days. 0 = show all. |
| `--min-size` | 500MB | Hide entries smaller than this. Accepts MB, GB. |
| `--sort` | size | Sort by `size`, `accessed`, or `dust` (composite size × age score). |
| `--top` | 40 | Number of results to show. |
| `--exclude` | | Exclude paths matching pattern (repeatable, e.g. `Downloads/foo`, `.git`, `*.dmg`). |
| `--workers` | CPU count | Parallel workers for the filesystem walk. |
| `--apparent-size` | false | Use apparent file size instead of actual disk block usage. |

## How it works

Walks the filesystem in parallel using raw OS syscalls (`getattrlistbulk` on macOS, `getdents64` + `fstatat` on Linux). A home directory with ~14 million files scans in ~5s with warm disk cache, ~18s with cold. Live scan progress is shown on `stderr` when attached to a terminal.

All directories and individual files meeting `--min-size` are collected, redundant ancestor directories (where a single qualifying child accounts for ≥95% of the parent's size) are collapsed automatically, and the top N are printed as a flat list. Hardlinks are deduplicated automatically.

Access time comes from the newer of file `atime` and `mtime`.

Supports Linux and macOS.

## Example

```
$ diskdust --top 10
Scanning: /home/user
Filters:  min-size=500MB  top=10

     SIZE    ACCESSED      FILES  PATH
  181 GiB  2026-10-07    9333847  work/
  111 GiB  2026-10-06    7460101  work/keycloak-worktree/
   76 GiB  2026-10-03     758446  .cache/
   39 GiB  2026-10-07    1611335  go/
   37 GiB  2026-10-07    1610431  go/pkg/
   37 GiB  2026-10-07    1610430  go/pkg/mod/
   35 GiB  2026-10-03     447244  .cache/bazel/
   28 GiB  2026-10-06     444244  .cache/bazel/_bazel_user/53df18/
   25 GiB  2026-10-06     311896  .cache/bazel/_bazel_user/53df18/external/
    4 GiB  2026-10-07      80521  .local/

Scanned 13,733,289 files in 671 directories (402 GiB) in 4.5s
Listed 10 directories (602 GiB)
```
