-- 极简 JSON 序列化。面板只需要读，不需要在 Lua 侧解析 JSON，
-- 因此这里只实现 encode，命令通道用的是竖线分隔的行格式，避免在游戏沙箱里写解析器。
local M = {}

local function is_array(t)
    local n = 0
    for k in pairs(t) do
        if type(k) ~= "number" then
            return false
        end
        n = n + 1
    end
    if n == 0 then
        return false
    end
    for i = 1, n do
        if t[i] == nil then
            return false
        end
    end
    return true
end

local control_chars = {
    ["\\"] = "\\\\",
    ['"'] = '\\"',
    ["\b"] = "\\b",
    ["\f"] = "\\f",
    ["\n"] = "\\n",
    ["\r"] = "\\r",
    ["\t"] = "\\t",
}

local function escape(s)
    local out = ""
    for i = 1, #s do
        local c = s:sub(i, i)
        local rep = control_chars[c]
        if rep then
            out = out .. rep
        elseif c:byte() < 32 then
            out = out .. string.format("\\u%04x", c:byte())
        else
            out = out .. c
        end
    end
    return out
end

local encode

-- 空表在 Lua 里既可能是空数组也可能是空字典，pairs 遍历不出来就没法区分。
-- 快照里的列表字段（在线玩家）用 array() 包一下，保证 0 个玩家时也编码成 []，
-- 否则面板侧 JSON 反序列化会因为类型对不上直接失败。
local array_mt = {__json_array = true}

function M.array(t)
    return setmetatable(t or {}, array_mt)
end

local function encode_table(v)
    local forced = getmetatable(v) == array_mt
    if forced or is_array(v) then
        local parts = {}
        for i = 1, #v do
            parts[i] = encode(v[i])
        end
        return "[" .. table.concat(parts, ",") .. "]"
    end
    local keys = {}
    for k in pairs(v) do
        keys[#keys + 1] = k
    end
    -- 固定顺序：面板不做 diff 比对，但键序稳定能让人工排查日志时省心。
    table.sort(keys, function(a, b) return tostring(a) < tostring(b) end)
    local parts = {}
    for _, k in ipairs(keys) do
        parts[#parts + 1] = '"' .. escape(tostring(k)) .. '":' .. encode(v[k])
    end
    return "{" .. table.concat(parts, ",") .. "}"
end

encode = function(v)
    local t = type(v)
    if t == "nil" then
        return "null"
    elseif t == "boolean" then
        return v and "true" or "false"
    elseif t == "number" then
        if v ~= v or v == math.huge or v == -math.huge then
            return "null" -- NaN/Inf 不是合法 JSON
        end
        if math.floor(v) == v and math.abs(v) < 1e15 then
            return string.format("%d", v)
        end
        return string.format("%.6g", v)
    elseif t == "string" then
        return '"' .. escape(v) .. '"'
    elseif t == "table" then
        return encode_table(v)
    end
    return "null"
end

function M.encode(v)
    return encode(v)
end

return M
