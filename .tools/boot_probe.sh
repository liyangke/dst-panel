#!/bin/bash
# 对着真进程打接口，逐个看返回码和响应体。
P=8931
PW=boot-check-pw
u() { curl -s -w "\nHTTP %{http_code}\n" -u "admin:$PW" "http://127.0.0.1:$P$1"; }
code() { curl -s -o /dev/null -w "%{http_code}" "$2" -u "admin:$PW" "http://127.0.0.1:$P$1"; }

echo "--- healthz（免认证）---"
curl -s --retry 15 --retry-delay 1 --retry-connrefused -w "\nHTTP %{http_code}\n" "http://127.0.0.1:$P/healthz"
echo "--- /api 无凭据（期望 401）---"
curl -s -o /dev/null -w "HTTP %{http_code}\n" "http://127.0.0.1:$P/api/v1/clusters"
echo "--- /api 错密码（期望 401）---"
curl -s -o /dev/null -w "HTTP %{http_code}\n" -u "admin:wrong" "http://127.0.0.1:$P/api/v1/clusters"
echo "--- clusters ---"
u /api/v1/clusters
echo "--- tasks ---"
u /api/v1/tasks
echo "--- tasks?limit=abc（期望 400）---"
u "/api/v1/tasks?limit=abc"
echo "--- 分片状态：Master（真快照，看中文标签）---"
u /api/v1/clusters/Cluster_1/shards/Master/status
echo "--- 分片状态：Caves（MOD 采集报错）---"
u /api/v1/clusters/Cluster_1/shards/Caves/status
echo "--- 分片状态：不存在的分片（期望 503 且无英文原文）---"
u /api/v1/clusters/Cluster_1/shards/Nope/status
echo "--- DELETE clusters（期望 405）---"
curl -s -o /dev/null -w "HTTP %{http_code}\n" -X DELETE -u "admin:$PW" "http://127.0.0.1:$P/api/v1/clusters"
echo "--- 不存在的.path（期望 401 而不是 404，说明认证在路由前）---"
curl -s -o /dev/null -w "HTTP %{http_code}\n" "http://127.0.0.1:$P/api/v1/nope"
