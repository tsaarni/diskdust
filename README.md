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
diskdust                            # biggest directories
diskdust --not-accessed 365         # only dirs not accessed in 365 days
diskdust --sort accessed            # sort by least recently accessed
diskdust ~/.cache --min-size 100MB  # focus on a specific directory
```

| Flag | Default | Description |
|------|---------|-------------|
| `--not-accessed` | 0 | Only show directories not accessed in this many days. 0 = show all. |
| `--min-size` | 500MB | Hide directories smaller than this. Accepts MB, GB. |
| `--sort` | size | Sort by `size` or `accessed` (largest or least recently accessed first). |
| `--top` | 40 | Number of results to show. |
| `--workers` | CPU count | Parallel workers for the filesystem walk. |

## How it works

Walks the filesystem in parallel using raw Linux syscalls. A home directory with ~14 million files scans in ~5s with warm disk cache, ~18s with cold.

All directories meeting `--min-size` are collected, sorted, and the top N are printed as a flat list. 

Access time comes from file `atime`. 

Linux only.

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
