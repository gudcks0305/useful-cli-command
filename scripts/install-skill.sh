#!/bin/sh
set -eu

usage() {
  echo "usage: sh scripts/install-skill.sh [--no-claude]"
}

link_claude=1
case "${1:-}" in
  "") ;;
  --no-claude) link_claude=0 ;;
  -h|--help)
    usage
    exit 0
    ;;
  *)
    usage >&2
    exit 2
    ;;
esac

if [ "$#" -gt 1 ]; then
  usage >&2
  exit 2
fi

if [ -z "${AGENTS_SKILLS_DIR:-}" ] && [ -z "${HOME:-}" ]; then
  echo "install-skill: HOME or AGENTS_SKILLS_DIR is required" >&2
  exit 1
fi
if [ "$link_claude" -eq 1 ] && [ -z "${CLAUDE_SKILLS_DIR:-}" ] && [ -z "${HOME:-}" ]; then
  echo "install-skill: HOME or CLAUDE_SKILLS_DIR is required" >&2
  exit 1
fi

canonical_directory() {
  directory=$1
  case "$directory" in
    /*) ;;
    *)
      echo "install-skill: directory must be absolute: $directory" >&2
      return 1
      ;;
  esac
  mkdir -p "$directory"
  CDPATH='' cd -- "$directory" && pwd -P
}

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd -P)
repo_root=$(CDPATH='' cd -- "$script_dir/.." && pwd -P)
source_dir="$repo_root/.agents/skills/useful-cli"
agents_skills_dir=$(canonical_directory "${AGENTS_SKILLS_DIR:-"$HOME/.agents/skills"}")
destination="$agents_skills_dir/useful-cli"

if [ ! -f "$source_dir/SKILL.md" ]; then
  echo "install-skill: bundled SKILL.md not found: $source_dir" >&2
  exit 1
fi
if [ -n "$(find "$source_dir" -type l -print -quit)" ]; then
  echo "install-skill: bundled skill must not contain symlinks" >&2
  exit 1
fi
if [ -L "$destination" ] || { [ -e "$destination" ] && [ ! -d "$destination" ]; }; then
  echo "install-skill: destination is not a managed directory: $destination" >&2
  exit 1
fi
if [ -d "$destination" ] && [ -n "$(find "$destination" -type l -print -quit)" ]; then
  echo "install-skill: destination contains symlinks; refusing update: $destination" >&2
  exit 1
fi
if [ -d "$destination" ] && [ ! -f "$destination/.useful-cli-managed" ]; then
  if ! diff -qr "$source_dir" "$destination" >/dev/null 2>&1; then
    echo "install-skill: destination is unmanaged or modified: $destination" >&2
    exit 1
  fi
fi

if [ "$link_claude" -eq 1 ]; then
  claude_skills_dir=$(canonical_directory "${CLAUDE_SKILLS_DIR:-"$HOME/.claude/skills"}")
  claude_destination="$claude_skills_dir/useful-cli"
  if [ -e "$claude_destination" ] || [ -L "$claude_destination" ]; then
    if [ ! -L "$claude_destination" ]; then
      echo "install-skill: Claude destination conflicts: $claude_destination" >&2
      exit 1
    fi
    if [ -e "$destination" ]; then
      if [ ! "$claude_destination" -ef "$destination" ]; then
        echo "install-skill: Claude destination conflicts: $claude_destination" >&2
        exit 1
      fi
    elif [ "$(readlink "$claude_destination")" != "$destination" ]; then
      echo "install-skill: Claude destination conflicts: $claude_destination" >&2
      exit 1
    fi
  fi
fi

umask 022
state_root=$(canonical_directory "$(dirname -- "$agents_skills_dir")/.skill-install")
transaction=$(mktemp -d "$state_root/useful-cli.XXXXXX")
stage="$transaction/new"
backup="$transaction/old"
backup_moved=0

cleanup() {
  if [ -n "${transaction:-}" ] && [ -d "$transaction" ]; then
    if [ "$backup_moved" -eq 1 ] && [ ! -e "$destination" ] && [ -d "$backup" ]; then
      if mv "$backup" "$destination"; then
        backup_moved=0
      else
        echo "install-skill: rollback failed; recovery copy: $backup" >&2
      fi
    fi
    if [ "$backup_moved" -eq 0 ]; then
      rm -rf "$transaction"
    fi
  fi
}
trap cleanup 0

mkdir "$stage"
cp -R "$source_dir/." "$stage/"
printf '%s\n' "managed by scripts/install-skill.sh" > "$stage/.useful-cli-managed"

if [ -d "$destination" ]; then
  mv "$destination" "$backup"
  backup_moved=1
fi
if ! mv "$stage" "$destination"; then
  echo "install-skill: publish failed: $destination" >&2
  exit 1
fi
if [ "$backup_moved" -eq 1 ]; then
  backup_moved=0
  rm -rf "$backup"
fi
rm -rf "$transaction"
transaction=

if [ "$link_claude" -eq 1 ]; then
  if [ ! -e "$claude_destination" ] && [ ! -L "$claude_destination" ]; then
    ln -s "$destination" "$claude_destination"
  fi
fi

echo "Installed useful-cli -> $destination"
if [ "$link_claude" -eq 1 ]; then
  echo "Linked Claude skill -> $claude_destination"
fi
