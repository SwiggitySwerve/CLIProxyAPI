"""Tests for proxd-claude-ua-sync's managed block in the Claude hub's config.yaml.
Run: python3 -I deploy/local/sidecars/claude-ua-sync/test_claude_ua_sync.py

Nothing here runs claude, podman or systemctl: the probe and every subprocess are stubbed.
"""
import importlib.machinery
import importlib.util
import sys
import tempfile
import unittest
import unittest.mock
from pathlib import Path

HERE = Path(__file__).resolve().parent
sys.dont_write_bytecode = True  # the loader would leave a .pyc beside the script
loader = importlib.machinery.SourceFileLoader("ua_sync", str(HERE / "proxd-claude-ua-sync"))
ua = importlib.util.module_from_spec(importlib.util.spec_from_loader(loader.name, loader))
loader.exec_module(ua)


def old_block(version):
    return ("# BEGIN cli-proxy-claude-ua-sync (managed, do not edit)\n"
            "# 2026-11-17T20:44:00Z: measured from /home/u/.local/share/claude/versions/x\n"
            "upstream:\n    claude:\n        header-defaults:\n"
            f'            user-agent: "claude-cli/{version} (external, cli)"\n'
            "# END cli-proxy-claude-ua-sync\n")


BEFORE = "port: 8317\nrouting:\n    strategy: soonest-reset\n"
AFTER = "observability:\n    logs:\n        error-logs-max-files: 500\n"
NEW_BLOCK = "# BEGIN proxd-claude-ua-sync (managed, do not edit)\nupstream: {}\n# END proxd-claude-ua-sync\n"


class ManagedBlock(unittest.TestCase):
    def test_old_marker_block_is_replaced_where_it_stands(self):
        text = BEFORE + old_block("2.1.290") + AFTER
        self.assertEqual(ua.current_baseline(text), "2.1.290")
        self.assertEqual(ua.with_block(text, NEW_BLOCK), BEFORE + NEW_BLOCK + AFTER)

    def test_new_marker_block_is_replaced(self):
        text = BEFORE + NEW_BLOCK.replace("{}", "x") + AFTER
        self.assertEqual(ua.with_block(text, NEW_BLOCK), BEFORE + NEW_BLOCK + AFTER)

    def test_both_marker_names_leave_one_block(self):
        text = BEFORE + old_block("2.1.290") + AFTER + NEW_BLOCK.replace("{}", "x")
        out = ua.with_block(text, NEW_BLOCK)
        self.assertEqual(out, BEFORE + NEW_BLOCK + AFTER)
        self.assertEqual(out.count("# BEGIN "), 1)

    def test_markers_count_only_as_whole_lines_with_matching_names(self):
        mention = "# notes: # BEGIN proxd-claude-ua-sync is managed\n"
        self.assertEqual(ua.with_block(BEFORE + mention + AFTER, NEW_BLOCK), BEFORE + mention + AFTER + NEW_BLOCK)
        mixed = old_block("2.1.290").replace("# END cli-proxy-", "# END proxd-")
        self.assertIsNone(ua.BLOCK_RE.search(mixed))
        stray = "# BEGIN proxd-claude-ua-sync (managed, do not edit)\n"  # no END: never part of a block
        self.assertEqual(ua.with_block(stray + BEFORE + old_block("2.1.290") + AFTER, NEW_BLOCK),
                         stray + BEFORE + NEW_BLOCK + AFTER)
        self.assertIsNone(ua.BLOCK_RE.search(old_block("2.1.290").replace("ua-sync (managed", "ua-sync-x (managed")))

    def test_no_block_is_appended_and_baseline_is_the_image_default(self):
        self.assertEqual(ua.current_baseline(BEFORE), ua.BUILTIN_BASELINE)
        self.assertEqual(ua.with_block(BEFORE + "\n\n", NEW_BLOCK), BEFORE + NEW_BLOCK)


class MainRewritesAnOldBlock(unittest.TestCase):
    def run_main(self, text, measured):
        with tempfile.TemporaryDirectory() as tmp:
            config = Path(tmp) / "config.yaml"
            config.write_text(text)
            headers = {"user-agent": f"claude-cli/{measured} (external, cli)",
                       "x-stainless-package-version": "0.99.0", "x-stainless-runtime-version": "v24.1.0"}
            hot_reload = unittest.mock.Mock(stdout="configuration updated", stderr="")
            with unittest.mock.patch.object(ua, "CONFIG", config), \
                    unittest.mock.patch.object(ua, "probe", return_value=headers), \
                    unittest.mock.patch.object(ua.subprocess, "run", return_value=hot_reload) as run, \
                    unittest.mock.patch.object(ua.time, "sleep"), \
                    unittest.mock.patch.object(sys, "argv", ["proxd-claude-ua-sync", "--no-update"]), \
                    unittest.mock.patch("builtins.print"):
                ua.main()
            commands = [c.args[0][:2] for c in run.call_args_list]
            self.backup_modes = [p.stat().st_mode & 0o777 for p in Path(tmp).glob("config.yaml.bak-uasync-*")]
            return config.read_text(), commands

    def test_newer_release_rewrites_the_old_block_with_new_markers_and_no_restart(self):
        out, commands = self.run_main(BEFORE + old_block("2.1.290") + AFTER, "2.1.300")
        self.assertTrue(out.startswith(BEFORE + "# BEGIN proxd-claude-ua-sync (managed, do not edit)\n"))
        self.assertTrue(out.endswith("# END proxd-claude-ua-sync\n" + AFTER))
        self.assertEqual(out.count("ua-sync"), 2)  # one BEGIN and one END, none with the old name
        self.assertIn('user-agent: "claude-cli/2.1.300 (external, cli)"', out)
        self.assertNotIn(["systemctl", "--user"], commands)  # the hot reload was seen
        self.assertEqual(self.backup_modes, [0o600])  # the backup holds the api keys

    def test_up_to_date_old_block_is_left_alone(self):
        text = BEFORE + old_block("2.1.300") + AFTER
        out, commands = self.run_main(text, "2.1.300")
        self.assertEqual(out, text)
        self.assertEqual(commands, [])


if __name__ == "__main__":
    unittest.main()
