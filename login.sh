#!/usr/bin/env bash
# login.sh — WorkBuddy CN OAuth 登录 → 落盘 auth 文件
#
# 用法:
#   ./login.sh
#
# 流程:
#   1. POST /v2/plugin/auth/state 拿授权 URL（无 PKCE，state 由服务端签发）
#   2. 你在浏览器打开 URL 完成登录
#   3. 回到这里按 y → poll 拿 token+uid+nickname → 签到 → 落盘 auths/workbuddy-<uid>.json
#   4. 重启 workbuddy2api 容器加载新账号
set -euo pipefail

cd "$(dirname "$0")"
AUTH_DIR="./auths"
CONTAINER="workbuddy2api"

mkdir -p "$AUTH_DIR"

# login 工具：缺失、或任一构建输入（*.go / go.mod / go.sum）比它新时重编。
#
# 为什么不能只在"不存在时"编译：源码改动后本地二进制会静默停留在旧版本。
# 这里尤其危险——realm 路由逻辑在 login 二进制里，过期的二进制会把 global 凭证
# 打向 CN 端点，症状与「token 过期」无法区分（issue #191 同类）。
# `-print -quit` 让 find 命中即退出，故 `set -o pipefail` 下不会因 grep 提前关闭
# 管道而吃到 find 的 SIGPIPE（141）。
LOGIN_BIN="./login"
if [[ ! -x "$LOGIN_BIN" ]] || find . \( -name '*.go' -o -name 'go.mod' -o -name 'go.sum' \) -newer "$LOGIN_BIN" -print -quit | grep -q .; then
    if ! command -v go >/dev/null 2>&1; then
        echo "需要 go 构建 login（或镜像内置 /app/login）" >&2
        exit 1
    fi
    echo "build login ..." >&2
    go build -o "$LOGIN_BIN" ./cmd/login
fi

echo "============================================================"
echo "  WorkBuddy OAuth 登录"
echo "============================================================"
echo ""

AUTH_URL=$("$LOGIN_BIN" url)

echo "请在浏览器中打开以下链接完成登录："
echo ""
echo "  $AUTH_URL"
echo ""

if command -v xclip &>/dev/null; then
    echo -n "$AUTH_URL" | xclip -selection clipboard 2>/dev/null && echo "(已复制到剪贴板)"
elif command -v xsel &>/dev/null; then
    echo -n "$AUTH_URL" | xsel --clipboard 2>/dev/null && echo "(已复制到剪贴板)"
fi

echo ""
read -rp "完成登录后按 y 继续: " ans
if [[ "$ans" != "y" && "$ans" != "Y" ]]; then
    echo "已取消"
    exit 1
fi

echo ""
echo "正在获取 token..."

RESULT=$("$LOGIN_BIN" poll) || {
    echo ""
    echo "获取 token 失败。可能原因："
    echo "  - 登录还没完成就按了 y（重新运行 ./login.sh 再试）"
    echo "  - 登录页报错（把报错截图发出来排查）"
    exit 1
}

TOKEN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['access_token'])")
REFRESH=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['refresh_token'])")
EXPIRES_IN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin)['expires_in'])")
DOMAIN=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('domain',''))")
USER_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('uid',''))")
ENT_ID=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('enterprise_id',''))")
NICKNAME=$(echo "$RESULT" | python3 -c "import json,sys; print(json.load(sys.stdin).get('nickname',''))")

if [[ -z "$USER_ID" ]]; then
    echo "无法获取 uid，请检查 token 是否有效"
    exit 1
fi

EXPIRES_AT=$(( $(date +%s) + EXPIRES_IN ))

# ─── 签到（CN：POST codebuddy.cn/v2/billing/meter/daily-checkin，幂等不阻塞）───
# OAuth 返回字段一律通过环境变量传入 Python，并用带引号 heredoc：
# 昵称/domain/token 等值可含引号或换行，拼进 Python 源码会造成注入。
WB2A_LOGIN_TOKEN="$TOKEN" \
WB2A_LOGIN_USER_ID="$USER_ID" \
WB2A_LOGIN_ENT_ID="$ENT_ID" \
WB2A_LOGIN_DOMAIN="$DOMAIN" \
python3 - <<'PYEOF'
import json, os, urllib.request, urllib.error

token = os.environ["WB2A_LOGIN_TOKEN"]
user_id = os.environ["WB2A_LOGIN_USER_ID"]
ent_id = os.environ["WB2A_LOGIN_ENT_ID"]
domain = os.environ["WB2A_LOGIN_DOMAIN"]

req = urllib.request.Request(
    "https://www.codebuddy.cn/v2/billing/meter/daily-checkin",
    method="POST", data=b"{}",
    headers={
        "Authorization": "Bearer " + token,
        "Accept": "application/json",
        "Content-Type": "application/json",
        "X-User-Id": user_id,
        **({"X-Enterprise-Id": ent_id, "X-Tenant-Id": ent_id} if ent_id else {}),
        **({"X-Domain": domain} if domain else {}),
    })
try:
    with urllib.request.urlopen(req, timeout=15) as r:
        body = json.loads(r.read().decode() or "{}")
    if body.get("code") == 0:
        data = body.get("data") or {}
        print(f"签到: 成功 {json.dumps(data, ensure_ascii=False)[:150]}")
    else:
        print(f"签到: {body.get('msg', json.dumps(body)[:150])}")
except urllib.error.HTTPError as e:
    # 已签到等业务错误也走 4xx（实测 code=10001 "今天已签到"）
    try:
        body = json.loads(e.read().decode() or "{}")
        print(f"签到: {body.get('msg', 'http %d' % e.code)}")
    except Exception:
        print(f"签到: http {e.code}")
except Exception as e:
    print(f"签到: {e}")
PYEOF

# ─── 落盘 auth 文件（与 internal/auth 读取格式一致）─────────────────
AUTH_FILE="$AUTH_DIR/workbuddy-${USER_ID}.json"
if [[ -f "$AUTH_FILE" ]]; then
    echo "账号已存在（uid=${USER_ID}），将覆盖更新凭证"
    ACTION="覆盖"
else
    echo "新账号（uid=${USER_ID}），新增 auth 文件"
    ACTION="新增"
fi
WB2A_LOGIN_TOKEN="$TOKEN" \
WB2A_LOGIN_REFRESH="$REFRESH" \
WB2A_LOGIN_EXPIRES_AT="$EXPIRES_AT" \
WB2A_LOGIN_DOMAIN="$DOMAIN" \
WB2A_LOGIN_USER_ID="$USER_ID" \
WB2A_LOGIN_ENT_ID="$ENT_ID" \
WB2A_LOGIN_NICKNAME="$NICKNAME" \
WB2A_LOGIN_AUTH_FILE="$AUTH_FILE" \
WB2A_LOGIN_ACTION="$ACTION" \
python3 - <<'PYEOF'
# 原子写：先写同目录临时文件，fsync 后 os.replace 覆盖。
#
# 为什么必须原子（与 auths 目录热加载配套）：网关每 5s 轮询 auths/ 目录指纹并全量
# 重扫。若此处用 `open(target,"w")` 直写，写入中途目录里会短暂存在**半截 JSON**，
# 而 auth.LoadDir 对坏文件是「跳过」——那一次 reload 会把该 uid 从池中剔除
# （已由 internal/pool/watch_partial_test.go 固化该行为）。虽然下一次指纹变化会把它
# 加载回来（瞬时抖动而非永久丢失），但停用/冷却状态在剔除重建间不保证连续。
# tempfile 与目标同目录 + os.replace 是 POSIX 原子操作，读方永远看到完整旧文件或
# 完整新文件（Windows 上 os.replace 同样覆盖已存在文件）。
# 另：临时文件前缀用 "." 开头，避开 LoadDir 的 workbuddy*.json glob。
import json, os, tempfile

auth = {
    "account": {
        "uid": os.environ["WB2A_LOGIN_USER_ID"],
        "enterpriseId": os.environ["WB2A_LOGIN_ENT_ID"],
        "nickname": os.environ["WB2A_LOGIN_NICKNAME"],
    },
    "auth": {
        "accessToken": os.environ["WB2A_LOGIN_TOKEN"],
        "refreshToken": os.environ["WB2A_LOGIN_REFRESH"],
        "expiresAt": int(os.environ["WB2A_LOGIN_EXPIRES_AT"]),
        "domain": os.environ["WB2A_LOGIN_DOMAIN"],
    },
}

auth_file = os.environ["WB2A_LOGIN_AUTH_FILE"]
tmp_file = None
try:
    fd, tmp_file = tempfile.mkstemp(prefix=".workbuddy-auth-", dir=os.path.dirname(auth_file) or ".")
    with os.fdopen(fd, "w") as f:
        json.dump(auth, f, indent=1)
        f.flush()
        os.fsync(f.fileno())
    os.replace(tmp_file, auth_file)
    tmp_file = None
except Exception as e:
    # 写入失败给出可操作指引（目录不可写是部署时最常见的失败形态）。
    print(f"❌ 写入 {auth_file} 失败: {e}", file=__import__("sys").stderr)
    auth_dir = os.path.dirname(auth_file) or "."
    if not os.access(auth_dir, os.W_OK):
        print(f"    {auth_dir}/ 不可写。容器部署请执行：", file=__import__("sys").stderr)
        print(f"    chown -R 10001:10001 {auth_dir}", file=__import__("sys").stderr)
        print("    或在容器内登录（属主自动正确）：", file=__import__("sys").stderr)
        print("    docker compose exec -it wb2api bash -c './login.sh'", file=__import__("sys").stderr)
    raise
finally:
    if tmp_file is not None:
        try:
            os.unlink(tmp_file)
        except FileNotFoundError:
            pass
print(f"已保存（{os.environ['WB2A_LOGIN_ACTION']}）: {auth_file}")
PYEOF

# ─── 重启服务 ────────────────────────────────────────────
echo ""
if docker ps --format '{{.Names}}' | grep -q "^${CONTAINER}$"; then
    echo "重启 $CONTAINER 加载新账号..."
    docker restart "$CONTAINER" >/dev/null
    sleep 2
    # API_KEY 从 config.json 读取（该变量在脚本中未定义，fallback 仅为占位，不会通过鉴权）
    API_KEY=$(python3 -c "import json; print(json.load(open('config.json')).get('api_key',''))" 2>/dev/null)
    COUNT=$(curl -s http://127.0.0.1:7863/status -H "Authorization: Bearer ${API_KEY:-test_key}" 2>/dev/null | python3 -c "import json,sys; print(len(json.load(sys.stdin).get('accounts',[])))" 2>/dev/null || echo "?")
    echo "服务已重启，当前账号数: $COUNT"
else
    echo "容器 $CONTAINER 未运行，auth 文件已保存，下次启动自动加载"
fi

echo ""
echo "============================================================"
echo "  登录完成！"
echo "  UID: $USER_ID"
echo "  Nickname: ${NICKNAME:-（未获取到）}"
echo "  Token: ${TOKEN:0:30}..."
echo "  有效期: $(date -d "@$EXPIRES_AT" '+%Y-%m-%d %H:%M' 2>/dev/null || echo "$EXPIRES_AT")"
echo "============================================================"
