# Shared by the hack/ scripts. Source it; it changes to the repo root.
cd "$(dirname "${BASH_SOURCE[0]}")/.."

CLUSTER=browserjs
NS=browserjs-sessions
# Everything the scripts generate (kubeconfig, throwaway CA) lives here and
# is not committed. The cluster has its own kubeconfig, so yours is untouched.
LOCAL_DIR="$PWD/.local"
export KUBECONFIG="$LOCAL_DIR/kubeconfig"

# Building an image, creating the cluster and loading images all grow
# colima's disk on the Mac's data volume; a full volume corrupts it.
MIN_FREE_GB="${MIN_FREE_GB:-12}"
check_disk() {
  local volume=/System/Volumes/Data free
  [ -d "$volume" ] || volume=/
  free=$(df -Pk "$volume" | awk 'NR==2 {print int($4 / 1048576)}')
  if [ "$free" -lt "$MIN_FREE_GB" ]; then
    echo "only ${free} GB free on $volume; need ${MIN_FREE_GB} GB before $1. Stopping." >&2
    exit 1
  fi
  echo "disk: ${free} GB free before $1"
}
