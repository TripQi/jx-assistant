# -*- coding: utf-8 -*-
"""打包时精简 内置组件/wmcppkg：剔掉与共享便携运行时版本完全一致的第三方包。

为什么这么做：Windows-MCP 的依赖里有 16 个包和共享运行时同名，其中 14 个
版本逐字一致（numpy / Pillow / cryptography / pywin32 / comtypes …），
合计约 83MB 是纯重复。版本一致才动手，版本不同一律保留——多留几百 KB
换"不冒版本冲突的险"，划算。

删除依据是每个包自己的 RECORD（精确到文件），删完连 dist-info 一起走，
避免留下"元数据说有、实际没有"的半截状态。

实测：138.1MB → 55.2MB，MCP 握手 21 个工具齐全（Click/Type/Scroll/
Move/Shortcut/WaitFor/Snapshot/App）。

用法（由 打包安装包.ps1 调用）：
  python 精简内置组件.py --site <运行时 site-packages> --target <wmcppkg 目录>
"""

import argparse
import os
import re
import shutil
import sys


def dir_size(path):
    total = 0
    for dirpath, _dirnames, filenames in os.walk(path):
        for f in filenames:
            try:
                total += os.path.getsize(os.path.join(dirpath, f))
            except OSError:
                pass
    return total


def dist_versions(site_dir):
    """扫 <包>-<版本>.dist-info，返回 {规范化包名: 版本}。"""
    out = {}
    if not os.path.isdir(site_dir):
        return out
    for name in os.listdir(site_dir):
        m = re.match(r"^(.+?)-(\d[^-]*)\.dist-info$", name)
        if m:
            out[m.group(1).lower().replace("_", "-")] = m.group(2)
    return out


def strip_by_record(target_dir, dist_info_name):
    """按 RECORD 删除一个包的全部文件，再删 dist-info 目录。返回删除字节数。"""
    rec = os.path.join(target_dir, dist_info_name, "RECORD")
    freed = 0
    if os.path.isfile(rec):
        with open(rec, encoding="utf-8", errors="ignore") as fh:
            for line in fh:
                rel = line.split(",")[0].strip().replace("/", os.sep)
                if not rel:
                    continue
                path = os.path.join(target_dir, rel)
                if os.path.isfile(path):
                    try:
                        freed += os.path.getsize(path)
                        os.remove(path)
                    except OSError:
                        pass
    before = dir_size(os.path.join(target_dir, dist_info_name))
    shutil.rmtree(os.path.join(target_dir, dist_info_name), ignore_errors=True)
    return freed + before


def prune_empty_dirs(root):
    for dirpath, _dirnames, _filenames in os.walk(root, topdown=False):
        if os.path.abspath(dirpath) == os.path.abspath(root):
            continue
        try:
            if not os.listdir(dirpath):
                os.rmdir(dirpath)
        except OSError:
            pass


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--site", required=True, help="共享运行时的 site-packages")
    ap.add_argument("--target", required=True, help="要精简的 wmcppkg 目录")
    ap.add_argument("--dry-run", action="store_true", help="只报告，不删")
    args = ap.parse_args()

    if not os.path.isdir(args.target):
        print("[去重] 目标不存在，跳过: %s" % args.target)
        return 0
    runtime_versions = dist_versions(args.site)
    if not runtime_versions:
        print("[去重] 共享运行时里没扫到 dist-info，跳过（不做任何删除）: %s" % args.site)
        return 0

    before = dir_size(args.target)
    removed, kept = [], []
    for name in sorted(os.listdir(args.target)):
        m = re.match(r"^(.+?)-(\d[^-]*)\.dist-info$", name)
        if not m:
            continue
        key = m.group(1).lower().replace("_", "-")
        ver = m.group(2)
        if runtime_versions.get(key) != ver:
            kept.append(key)
            continue
        if args.dry_run:
            removed.append("%s %s" % (key, ver))
            continue
        strip_by_record(args.target, name)
        removed.append("%s %s" % (key, ver))

    if not args.dry_run:
        prune_empty_dirs(args.target)
    after = dir_size(args.target)

    print("[去重] 移除 %d 个与运行时同版本的包：%s" % (len(removed), ", ".join(removed)))
    print("[去重] 保守保留 %d 个版本不同/运行时没有的包" % len(kept))
    print("[去重] %.1f MB → %.1f MB（省 %.1f MB）"
          % (before / 1048576.0, after / 1048576.0, (before - after) / 1048576.0))
    return 0


if __name__ == "__main__":
    sys.exit(main())
