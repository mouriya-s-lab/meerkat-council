#!/bin/sh
# Builds the council's omp agent dir for a local run.
#
# usage: prepare-omp-dir.sh <config-root>
#
# Reads omp.agentDir from <config-root>/council/runtime.yaml with yq (mikefarah/yq v4, required),
# copies <config-root>/council/omp/config.yml to <agentDir>/config.yml and links the
# credential store <agentDir>/agent.db{,-wal,-shm} and the custom provider definitions
# <agentDir>/models.yml (when present) to the same files in ~/.omp/agent/.
set -eu

if [ "$#" -ne 1 ]; then
	echo "usage: $0 <config-root>" >&2
	exit 2
fi

root=$1
runtime="$root/council/runtime.yaml"
source_config="$root/council/omp/config.yml"

command -v yq >/dev/null || { echo "prepare-omp-dir: yq (mikefarah/yq v4) is required" >&2; exit 1; }
[ -f "$runtime" ] || { echo "prepare-omp-dir: $runtime not found" >&2; exit 1; }
[ -f "$source_config" ] || { echo "prepare-omp-dir: $source_config not found" >&2; exit 1; }

agent_dir=$(yq -e '.omp.agentDir' "$runtime")
case "$agent_dir" in
	/*) ;;
	*) echo "prepare-omp-dir: omp.agentDir must be absolute, got '$agent_dir'" >&2; exit 1 ;;
esac

mkdir -p "$agent_dir"
cp "$source_config" "$agent_dir/config.yml"
for name in agent.db agent.db-wal agent.db-shm; do
	ln -sfn "$HOME/.omp/agent/$name" "$agent_dir/$name"
done
if [ -f "$HOME/.omp/agent/models.yml" ]; then
	ln -sfn "$HOME/.omp/agent/models.yml" "$agent_dir/models.yml"
fi
echo "prepared $agent_dir"
