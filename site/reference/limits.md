# Limits

| What | Limit |
| --- | --- |
| Sessions per account | 5. Delete one to create another |
| Session name | 1 to 63 characters, no control characters |
| Idle time before sleep | 15 minutes |
| Wait for a waking session | Up to 5 minutes. After that a call answers `504` and can be retried |
| Session disk | 5 GB, shared by the browser profile, downloads and agent memory. The same at every size |
| Desktop CPU and memory | By [size](/reference/session-sizes): small 1.5 CPU and 2 GiB, medium 2 CPU and 5 GiB, large 3 CPU and 10 GiB |
| Sessions awake at once, all users together | One large, or three medium. Small ones fill what is left. Past that, a create or a wake answers `409` |
| File sent or saved through the Files box | 100 MB each |
| File name | One name, not a path. Up to 255 bytes. No leading dot |
| Time to move one file | 5 minutes |
| Desktop size | 320 by 200 up to 2560 by 1600 |
| `run_js` run time | 30 seconds by default, up to 300 |
| `run_js` heap | 8 MB by default (16 MB in a medium session, 32 MB in a large one); up to 64 MB when a call asks |
| Artifact | 16 MiB each |
| Upload through `get_artifact_upload_url` | 16 MiB, URL valid for 10 minutes, once |

These are the limits today. They may change.
