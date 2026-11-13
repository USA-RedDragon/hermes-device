package.path = "docs/site/?.lua;" .. package.path
local nav = require("nav")

local n = nav.load()
for _, it in ipairs(n.missing) do
  io.stderr:write("not yet written: docs/" .. it.planned .. " (" .. it.title .. ")\n")
end
for _, it in ipairs(n.pages) do
  if it.moved then
    io.stderr:write("old path: " .. it.out .. " comes from " .. it.moved .. " until docs/" .. it.target .. " exists\n")
  end
  print(it.src .. "\t" .. it.out)
end
for _, p in ipairs(nav.extra(n)) do
  local out = nav.default_out(p)
  if n.by_out[out] then
    io.stderr:write("not in docs/nav.md: " .. p .. " (skipped: " .. out .. " is taken)\n")
  else
    io.stderr:write("not in docs/nav.md: " .. p .. "\n")
    print(p .. "\t" .. out)
  end
end
