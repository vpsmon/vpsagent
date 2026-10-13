#!/usr/bin/env python3
"""Test the release-generated installer with local fixtures, no client runtimes."""
import importlib.util
import os
from pathlib import Path
import subprocess
import tempfile

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location("build_install", ROOT / "scripts/build-install.py")
builder = importlib.util.module_from_spec(spec)
spec.loader.exec_module(builder)
TEMPLATE = (ROOT / "scripts/install.sh").read_text()


def run_case(mode, arch="amd64"):
    with tempfile.TemporaryDirectory(prefix="vpsagent-install-check-") as temporary:
        directory = Path(temporary)
        tools = directory / "tools"
        tools.mkdir()
        marker = directory / "binary-executed"
        forbidden = directory / "forbidden-runtime"
        binary = f'''#!/usr/bin/env bash
printf 'executed\\n' >> '{marker}'
if [[ $1 == version ]]; then
  printf '%s\\n' 'v1.2.3'
elif [[ $1 == connect && $2 == --help ]]; then
  printf '%s\\n' '  --setup-token-stdin'
elif [[ $1 == print-helper && ( $2 == update || $2 == remove ) ]]; then
  printf '%s\\n' '#!/usr/bin/env bash' 'exit 0'
else exit 2; fi
'''
        for target in ("amd64", "arm64", "arm", "386"):
            (directory / f"vpsagent-linux-{target}").write_text(binary)
        source = builder.render(TEMPLATE, "v1.2.3", directory)
        if mode == "unrendered":
            source = TEMPLATE
        elif mode == "bad-version":
            source = source.replace("RELEASE_TAG='v1.2.3'", "RELEASE_TAG='../bad'")
        elif mode == "bad-digest":
            # Change downloaded bytes after the installer pins are rendered.
            (directory / f"vpsagent-linux-{arch}").write_text(binary + "# tampered\n")
        source = source[source.index("# Verify before executing"):source.index('if [[ -z $SETUP_TOKEN ]]; then', source.index("# Verify before executing"))]
        source = source.replace("mktemp -d /opt/.vpsagent-install.XXXXXX", 'mktemp -d "$TEST_ROOT/.vpsagent-install.XXXXXX"')
        source = source.replace("df -Pk /opt", 'df -Pk "$TEST_ROOT"')
        assert '/opt/' not in source
        source = source.replace("trap 'rm -rf -- \"$TEMP_DIR\"' EXIT", "trap 'if [[ -f \"$TEMP_DIR/update.sh\" ]]; then cp \"$TEMP_DIR/update.sh\" \"$TEST_ROOT/update.out\"; fi; if [[ -f \"$TEMP_DIR/remove.sh\" ]]; then cp \"$TEMP_DIR/remove.sh\" \"$TEST_ROOT/remove.out\"; fi; rm -rf -- \"$TEMP_DIR\"' EXIT")
        (tools / "curl").write_text('''#!/usr/bin/env bash
set -euo pipefail
url= destination=
while [[ $# -gt 0 ]]; do
  case "$1" in
    https://*) url=$1; shift ;;
    -o) destination=$2; shift 2 ;;
    *) shift ;;
  esac
done
[[ $url == "https://github.com/vpsmon/vpsagent/releases/download/v1.2.3/vpsagent-linux-$GOARCH" ]] || exit 90
cp "$TEST_ROOT/vpsagent-linux-$GOARCH" "$destination"
''')
        (tools / "df").write_text('''#!/usr/bin/env bash
printf 'Filesystem 1024-blocks Used Available Capacity Mounted\\nfixture 9999999 0 %s 0%% /\\n' "${TEST_AVAILABLE_KIB:-9999999}"
''')
        for command in ("python", "python3", "cosign", "jq", "timeout"):
            (tools / command).write_text(f"#!/usr/bin/env bash\nprintf forbidden > '{forbidden}'\nexit 99\n")
        for executable in tools.iterdir():
            executable.chmod(0o755)
        harness = directory / "verify.sh"
        harness.write_text(source)
        env = {**os.environ, "PATH": str(tools) + os.pathsep + os.environ.get("PATH", ""), "APP_NAME": "vpsagent", "REPO": "vpsmon/vpsagent", "GOARCH": arch, "TEST_ROOT": str(directory)}
        if mode == "low-space":
            env["TEST_AVAILABLE_KIB"] = "327679"
        proc = subprocess.run(["bash", "-euo", "pipefail", str(harness)], env=env, capture_output=True, text=True)
        assert not forbidden.exists(), "installer invoked an unwanted runtime"
        if mode == "valid":
            assert proc.returncode == 0, proc.stderr
            assert marker.read_text().splitlines() == ["executed"] * 4
            for helper in ("update", "remove"):
                assert "exit 0" in (directory / f"{helper}.out").read_text()
        else:
            assert proc.returncode != 0, f"{mode}: accepted invalid fixture"
            assert not marker.exists(), f"{mode}: executed unverified binary"
        assert not list(directory.glob(".vpsagent-install.*")), "staging was not cleaned"


if __name__ == "__main__":
    for arch in ("amd64", "arm64", "arm", "386"):
        run_case("valid", arch)
        run_case("bad-digest", arch)
    for mode in ("unrendered", "bad-version", "low-space"):
        run_case(mode)
    print("pinned installer fixtures passed for all four architectures; no extra client runtime invoked")
