#!/bin/bash
# 看清本地那几个 tar/ssh 到底是什么命令、什么时候起的。
# 有两对 tar+ssh 同时在跑的话，它们会往同一个云端目录灌两条流，必须先确认。
powershell -NoProfile -Command "Get-CimInstance Win32_Process -Filter \"Name='tar.exe' OR Name='ssh.exe'\" | Sort-Object CreationDate | ForEach-Object { \$_.ProcessId.ToString() + '  ' + \$_.CreationDate.ToString() + '  ' + \$_.CommandLine }" 2>&1 | head -20
