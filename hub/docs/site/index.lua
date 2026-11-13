package.path = "docs/site/?.lua;" .. package.path
local navlib = require("nav")

local outdir = arg[1]
local dir = outdir .. "/.search"
local nav = navlib.load()
local order = {}
for i, it in ipairs(nav.pages) do
  order[it.out] = i
end
local entries = {}
for _, name in ipairs(pandoc.system.list_directory(dir)) do
  local data = navlib.read_file(dir .. "/" .. name)
  local e = pandoc.json.decode(data, false)
  table.insert(entries, e)
end
table.sort(entries, function(a, b)
  local x, y = order[a.u] or math.huge, order[b.u] or math.huge
  if x ~= y then
    return x < y
  end
  return a.u < b.u
end)
local f = assert(io.open(outdir .. "/search-index.js", "wb"))
f:write("window.siteSearch=" .. pandoc.json.encode(entries) .. ";\n")
f:close()
for _, name in ipairs(pandoc.system.list_directory(dir)) do
  os.remove(dir .. "/" .. name)
end
os.remove(dir)
io.stderr:write("search index: " .. #entries .. " pages\n")
