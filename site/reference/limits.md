# Limits

| What | Limit |
| --- | --- |
| Sessions per account | 5. Delete one to create another. |
| Session name | 1 to 63 characters, no control characters |
| Idle time before sleep | 15 minutes |
| Wait for a waking session | Up to 5 minutes. After that a call answers `504` and can be retried. |
| Session disk | 5 GB, shared by the browser profile, downloads and agent memory |
| File sent or saved through the Files box | 100 MB each |
| File name | One name, not a path. Up to 255 bytes. No leading dot. |
| Time to move one file | 5 minutes |
| Remote desktop size | 320 by 200 up to 2560 by 1600 |
| `run_js` run time | 30 seconds by default, up to 300 |
| `run_js` heap | 8 MB by default |
| Artifact | 16 MiB each |
| Upload through `get_artifact_upload_url` | 16 MiB, URL valid for 10 minutes, once |

These are the limits today. They may change.
