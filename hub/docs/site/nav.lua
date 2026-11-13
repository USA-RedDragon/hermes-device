local stringify = pandoc.utils.stringify

local M = {}

function M.read_file(p)
  local f = io.open(p, "rb")
  if not f then
    return nil
  end
  local data = f:read("a")
  f:close()
  return data
end

function M.is_dir(p)
  return pcall(pandoc.system.list_directory, p)
end

function M.resolve(dir, target)
  local parts = {}
  local whole = dir == "" and target or dir .. "/" .. target
  for seg in whole:gmatch("[^/]+") do
    if seg == ".." then
      if #parts == 0 then
        return nil
      end
      table.remove(parts)
    elseif seg ~= "." then
      table.insert(parts, seg)
    end
  end
  return table.concat(parts, "/")
end

function M.relative(from_dir, to)
  local a, b = {}, {}
  for s in from_dir:gmatch("[^/]+") do
    table.insert(a, s)
  end
  for s in to:gmatch("[^/]+") do
    table.insert(b, s)
  end
  local i = 1
  while i <= #a and i < #b and a[i] == b[i] do
    i = i + 1
  end
  local out = {}
  for _ = i, #a do
    table.insert(out, "..")
  end
  for j = i, #b do
    table.insert(out, b[j])
  end
  return table.concat(out, "/")
end

function M.dir_of(p)
  return p:match("^(.*)/[^/]*$") or ""
end

function M.default_out(src)
  if src == "README.md" then
    return "index.html"
  end
  local rest = src:match("^docs/(.*)$")
  if not rest then
    return nil
  end
  local d = rest:match("^(.*)/README%.md$")
  if d then
    return d .. ".html"
  end
  local p = rest:match("^(.*)%.md$")
  return p and p .. ".html" or nil
end

local function item_of(blocks)
  local plain = blocks[1]
  if not plain or (plain.t ~= "Plain" and plain.t ~= "Para") then
    error("docs/nav.md: an item has no text")
  end
  local link, code, old
  for _, x in ipairs(plain.content) do
    if x.t == "Link" and not link then
      link = x
    elseif x.t == "Code" and not code then
      code = x
    elseif x.t == "RawInline" and x.format == "html" then
      old = old or x.text:match("^<!%-%-%s*old:%s*(%S+)%s*%-%->$")
    end
  end
  local it = {}
  if link then
    it.title = stringify(link.content)
    it.target = link.target
  elseif code then
    local t = {}
    for _, x in ipairs(plain.content) do
      if x.t == "Code" then
        break
      end
      table.insert(t, x)
    end
    it.title = stringify(t):gsub("[:%s]+$", "")
    it.planned = code.text
  else
    it.title = stringify(plain.content)
  end
  it.old = old
  return it
end

-- load reads docs/nav.md and returns the sections, each with a title and
-- items, and a map from each source file to its output page. It runs from
-- the repository root.
function M.load()
  local text = M.read_file("docs/nav.md")
  if not text then
    error("docs/nav.md not found")
  end
  local doc = pandoc.read(text, "gfm")
  local list
  for _, b in ipairs(doc.blocks) do
    if b.t == "BulletList" then
      list = b
      break
    end
  end
  if not list then
    error("docs/nav.md has no list")
  end
  local nav = { sections = {}, pages = {}, by_src = {}, by_out = {}, missing = {} }
  local function add(it, section)
    it.section = section
    if it.planned then
      if not M.read_file("docs/" .. it.planned) then
        it.kind = "planned"
        it.out = M.default_out("docs/" .. it.planned)
        table.insert(nav.missing, it)
        return it
      end
      io.stderr:write("written: docs/" .. it.planned .. "; make it a link in docs/nav.md\n")
      it.target = it.planned
    end
    if not it.target then
      error("docs/nav.md: item " .. it.title .. " has no link and no path")
    end
    if it.target:match("^%a[%w+.-]*:") then
      it.kind = "external"
      return it
    end
    local p = M.resolve("docs", it.target)
    local out = p and M.default_out(p)
    if not out or not p:match("%.md$") then
      it.kind = "file"
      it.path = p
      return it
    end
    it.out = out
    local src = p
    if not M.read_file(src) and it.old then
      local o = M.resolve("docs", it.old)
      if o and M.read_file(o) then
        src = o
        it.moved = o
      end
    end
    if not M.read_file(src) then
      it.kind = "planned"
      it.planned = p:gsub("^docs/", "")
      table.insert(nav.missing, it)
      return it
    end
    it.kind = "page"
    it.src = src
    nav.by_src[src] = out
    nav.by_src[p] = out
    nav.by_out[out] = it
    table.insert(nav.pages, it)
    return it
  end
  for _, entry in ipairs(list.content) do
    local head = item_of(entry)
    local sub
    for _, b in ipairs(entry) do
      if b.t == "BulletList" then
        sub = b
      end
    end
    local section = { title = head.title, items = {} }
    if sub then
      for _, child in ipairs(sub.content) do
        table.insert(section.items, add(item_of(child), section))
      end
    else
      section.title = nil
      table.insert(section.items, add(head, section))
    end
    table.insert(nav.sections, section)
  end
  return nav
end

-- extra returns the Markdown files under docs/ that nav.md does not list,
-- other than nav.md, in path order.
function M.extra(nav)
  local out = {}
  local function walk(dir)
    for _, name in ipairs(pandoc.system.list_directory(dir)) do
      local p = dir .. "/" .. name
      if M.is_dir(p) then
        if p ~= "docs/site" then
          walk(p)
        end
      elseif name:match("%.md$") and p ~= "docs/nav.md" and not nav.by_src[p] then
        table.insert(out, p)
      end
    end
  end
  walk("docs")
  table.sort(out)
  return out
end

return M
