#!/bin/bash
# Name    : stop_script.py
# Date    : 2022.09.05
# Func    : 停止脚本
# Note    : 注意：当前路径为应用部署文件夹

#############################################################
# 用户自定义
process_name="dmdb_exporter"       # 进程名


# 停止进程
if [[ "${process_name}x" != "x" ]]; then
    killall ${process_name}
fi

exit 0
