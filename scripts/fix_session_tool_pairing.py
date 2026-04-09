#!/usr/bin/env python3
"""
修复 Claude Code session JSONL 中 tool_use/tool_result 配对问题。

Bedrock 严格要求每个 tool_use 后必须有对应的 tool_result。
如果 session 文件被外部工具修改导致配对断裂，本脚本可以修复。

用法:
  python3 scripts/fix_session_tool_pairing.py                    # 检查当前项目最新 session
  python3 scripts/fix_session_tool_pairing.py --fix              # 检查并修复
  python3 scripts/fix_session_tool_pairing.py /path/to/file.jsonl --fix  # 指定文件
"""

import json
import os
import sys
import copy
import glob
import shutil
from datetime import datetime


def find_latest_session():
    """找到当前项目最新的 session 文件"""
    patterns = [
        os.path.expanduser("~/.claude/projects/-Users-wowdd1-Dev-*/*.jsonl"),
        os.path.expanduser("~/.claude/projects/*/*.jsonl"),
    ]
    files = []
    for pattern in patterns:
        files.extend(glob.glob(pattern))
    if not files:
        return None
    return max(files, key=os.path.getmtime)


def scan_tool_pairing(filepath):
    """扫描 tool_use/tool_result 配对，返回孤立的 tool_use"""
    tool_uses = {}   # id -> (line_number, tool_name)
    tool_results = set()
    total_lines = 0

    with open(filepath, 'r', encoding='utf-8') as f:
        for i, line in enumerate(f):
            total_lines = i
            try:
                obj = json.loads(line)
            except (json.JSONDecodeError, ValueError):
                continue

            msg = obj.get('message', {})
            content = msg.get('content', [])
            if isinstance(content, list):
                for item in content:
                    if isinstance(item, dict):
                        if item.get('type') == 'tool_use':
                            tool_uses[item.get('id', '')] = (i, item.get('name', ''))
                        elif item.get('type') == 'tool_result':
                            tool_results.add(item.get('tool_use_id', ''))

    # 排除最后 10 行（可能是正在执行的工具调用）
    orphans = {
        tid: info for tid, info in tool_uses.items()
        if tid and tid not in tool_results and info[0] < total_lines - 10
    }
    return tool_uses, tool_results, orphans, total_lines


def fix_orphans(filepath, orphan_ids):
    """修复孤立的 tool_use：从 message.content 中移除"""
    backup = filepath + f".bak.{datetime.now().strftime('%Y%m%d%H%M%S')}"
    shutil.copy2(filepath, backup)
    print(f"备份: {backup}")

    lines = []
    with open(filepath, 'r', encoding='utf-8') as f:
        lines = f.readlines()

    fixed_count = 0
    new_lines = []
    for raw_line in lines:
        try:
            obj = json.loads(raw_line)
        except (json.JSONDecodeError, ValueError):
            new_lines.append(raw_line)
            continue

        msg = obj.get('message', {})
        content = msg.get('content', [])
        if isinstance(content, list):
            original_len = len(content)
            new_content = [
                item for item in content
                if not (isinstance(item, dict) and item.get('type') == 'tool_use' and item.get('id', '') in orphan_ids)
            ]
            if len(new_content) < original_len:
                fixed_count += (original_len - len(new_content))
                if new_content:
                    obj = copy.deepcopy(obj)
                    obj['message']['content'] = new_content
                    new_lines.append(json.dumps(obj, ensure_ascii=False) + '\n')
                else:
                    pass  # 跳过整行（content 为空）
                continue

        new_lines.append(raw_line)

    with open(filepath, 'w', encoding='utf-8') as f:
        f.writelines(new_lines)

    return fixed_count


def main():
    fix_mode = '--fix' in sys.argv
    args = [a for a in sys.argv[1:] if not a.startswith('--')]

    if args:
        filepath = args[0]
    else:
        filepath = find_latest_session()
        if not filepath:
            print("找不到 session 文件")
            sys.exit(1)

    print(f"文件: {filepath}")
    print(f"大小: {os.path.getsize(filepath) / 1024 / 1024:.1f} MB")

    tool_uses, tool_results, orphans, total_lines = scan_tool_pairing(filepath)

    print(f"总行数: {total_lines}")
    print(f"tool_use 总数: {len(tool_uses)}")
    print(f"tool_result 总数: {len(tool_results)}")
    print(f"孤立 tool_use: {len(orphans)}")

    if not orphans:
        print("\n✓ 配对完整，无需修复")
        return

    print("\n孤立的 tool_use:")
    for tid, (line_num, name) in sorted(orphans.items(), key=lambda x: x[1][0]):
        print(f"  Line {line_num}: {name} ({tid[:30]}...)")

    if fix_mode:
        print(f"\n正在修复...")
        fixed = fix_orphans(filepath, set(orphans.keys()))
        print(f"✓ 已移除 {fixed} 个孤立 tool_use")

        # 验证
        _, _, remaining, _ = scan_tool_pairing(filepath)
        if remaining:
            print(f"⚠ 仍有 {len(remaining)} 个孤立 tool_use")
        else:
            print("✓ 验证通过，配对完整")
    else:
        print(f"\n运行 --fix 来修复: python3 {sys.argv[0]} {filepath} --fix")


if __name__ == '__main__':
    main()
