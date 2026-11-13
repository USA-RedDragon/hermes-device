package.path = "docs/site/?.lua;" .. package.path
local navlib = require("nav")
local stringify = pandoc.utils.stringify
local read_file, resolve, relative, dir_of = navlib.read_file, navlib.resolve, navlib.relative, navlib.dir_of

local function html(s)
  return (s:gsub("&", "&amp;"):gsub("<", "&lt;"):gsub(">", "&gt;"):gsub('"', "&quot;"))
end

local function first_header(blocks, level)
  for _, b in ipairs(blocks) do
    if b.t == "Header" and b.level == level then
      return b
    end
  end
end

-- repo_url returns the address of the repository, from the first line of
-- docs/site/repo.
local function repo_url()
  local url = (read_file("docs/site/repo") or ""):match("^%s*(%S+)")
  if not url then
    error("docs/site/repo must hold the address of the repository")
  end
  return url
end

local function only_images(inlines)
  local n = 0
  for _, x in ipairs(inlines) do
    if x.t == "Image" then
      n = n + 1
    elseif x.t ~= "Space" and x.t ~= "SoftBreak" and x.t ~= "LineBreak" then
      return false
    end
  end
  return n > 0
end

local function blank(inlines)
  for _, x in ipairs(inlines) do
    if x.t ~= "Space" and x.t ~= "SoftBreak" and x.t ~= "LineBreak" then
      return false
    end
  end
  return true
end

local function out_path(p)
  return p:match("^docs/(.*)$") or p
end

local function write_file(p, data)
  pandoc.system.make_directory(dir_of(p), true)
  local f = assert(io.open(p, "wb"))
  f:write(data)
  f:close()
end

local function nav_html(nav, page, root, repo)
  local parts = {}
  for _, sec in ipairs(nav.sections) do
    local items = {}
    for _, it in ipairs(sec.items) do
      local href, attrs = nil, ""
      if it.kind == "page" then
        href = root .. it.out
        if it.out == page then
          attrs = ' class="on" aria-current="page"'
        end
      elseif it.kind == "external" then
        href = it.target
      elseif it.kind == "file" and it.path and (read_file(it.path) or navlib.is_dir(it.path)) then
        href = repo .. (navlib.is_dir(it.path) and "/tree/main/" or "/blob/main/") .. it.path
      end
      if href then
        table.insert(items, '<li><a href="' .. html(href) .. '"' .. attrs .. ">" .. html(it.title) .. "</a></li>")
      end
    end
    if #items > 0 then
      if sec.title then
        table.insert(parts, '<li class="sec"><span>' .. html(sec.title) .. "</span><ul>" .. table.concat(items) .. "</ul></li>")
      else
        table.insert(parts, table.concat(items))
      end
    end
  end
  return "<ul>" .. table.concat(parts) .. "</ul>"
end

local function toc_html(toc)
  if #toc < 2 then
    return nil
  end
  local items = {}
  for _, t in ipairs(toc) do
    table.insert(items, '<li class="l' .. t.level .. '"><a href="#' .. html(t.id) .. '">' .. html(t.title) .. "</a></li>")
  end
  return "<ul>" .. table.concat(items) .. "</ul>"
end

local function pager_html(nav, page, root)
  local idx
  for i, it in ipairs(nav.pages) do
    if it.out == page then
      idx = i
    end
  end
  if not idx then
    return nil
  end
  local out = {}
  local prev, nxt = nav.pages[idx - 1], nav.pages[idx + 1]
  if prev then
    table.insert(out, '<a class="prev" rel="prev" href="' .. html(root .. prev.out) .. '"><span>Previous</span>' .. html(prev.title) .. "</a>")
  end
  if nxt then
    table.insert(out, '<a class="next" rel="next" href="' .. html(root .. nxt.out) .. '"><span>Next</span>' .. html(nxt.title) .. "</a>")
  end
  if #out == 0 then
    return nil
  end
  return table.concat(out)
end

local function plain_text(blocks, limit)
  local s = stringify(pandoc.Pandoc(blocks)):gsub("%s+", " ")
  if #s > limit then
    s = s:sub(1, limit)
  end
  return s
end

function Pandoc(doc)
  local src = PANDOC_STATE.input_files[1]
  local here = dir_of(src)
  local outdir = stringify(doc.meta.outdir)
  local page = stringify(doc.meta.page)
  local page_dir = dir_of(page)
  local root = relative(page_dir, "x"):gsub("x$", "")
  local repo = repo_url()
  local nav = navlib.load()
  local readme = src == "README.md" and doc or pandoc.read(read_file("README.md"), "gfm")

  doc = doc:walk({
    Image = function(img)
      if img.src:match("^%a[%w+.-]*:") then
        return {}
      end
      local p = resolve(here, img.src)
      local data = p and read_file(p)
      if not data then
        error(src .. ": image " .. img.src .. " not found")
      end
      write_file(outdir .. "/" .. out_path(p), data)
      img.src = relative(page_dir, out_path(p))
      return img
    end,
    Link = function(l)
      if #l.content == 0 then
        return {}
      end
      local t = l.target
      if t:match("^%a[%w+.-]*:") or t:match("^#") then
        return l
      end
      local file, frag = t:match("^([^#]*)(#?.*)$")
      local p = resolve(here, file)
      if not p then
        return l
      end
      local target = nav.by_src[p]
      if not target and p:match("%.md$") and read_file(p) then
        target = navlib.default_out(p)
      end
      if target then
        l.target = relative(page_dir, target) .. frag
      elseif read_file(p) or navlib.is_dir(p) then
        l.target = repo .. (navlib.is_dir(p) and "/tree/main/" or "/blob/main/") .. p .. frag
      end
      return l
    end,
  })

  local toc, heads = {}, {}
  local h1
  doc = doc:walk({
    Para = function(para)
      if blank(para.content) then
        return {}
      end
      if only_images(para.content) then
        return pandoc.Div(pandoc.Plain(para.content), { class = "figs" })
      end
    end,
    Table = function(tbl)
      return pandoc.Div({ tbl }, { class = "table" })
    end,
    Header = function(h)
      local title = stringify(h)
      if h.level == 1 then
        h1 = h1 or title
        return h
      end
      if h.level <= 3 then
        table.insert(toc, { id = h.identifier, title = title, level = h.level })
      end
      table.insert(heads, { h.identifier, title })
      if h.identifier ~= "" then
        h.content:insert(pandoc.Link({ pandoc.Str("§") }, "#" .. h.identifier, "", { class = "self", ["aria-label"] = "Link to " .. title }))
      end
      return h
    end,
  })

  local site = stringify(first_header(readme.blocks, 1) or "")
  local description
  for i, b in ipairs(doc.blocks) do
    if b.t == "Para" then
      description = stringify(b)
      if page == "index.html" then
        doc.blocks[i] = pandoc.Div({ b }, { class = "lede" })
      end
      break
    end
  end

  local section
  local it = nav.by_out[page]
  if it and it.section then
    section = it.section.title
  end
  local title = h1 or site
  local entry = { u = page, t = page == "index.html" and site or title, s = section or "", h = heads, x = plain_text(doc.blocks, 6000) }
  write_file(outdir .. "/.search/" .. page:gsub("/", "__") .. ".json", pandoc.json.encode(entry))

  local raw = function(s) return pandoc.MetaBlocks({ pandoc.RawBlock("html", s) }) end
  local links = { '<a href="' .. repo .. '">GitHub</a>' }
  doc.meta.outdir = nil
  doc.meta.page = nil
  doc.meta.root = root
  doc.meta.site = site
  doc.meta.pagetitle = (page == "index.html" or not h1) and site or h1 .. " - " .. site
  doc.meta.description = description or site
  doc.meta.nav = raw(nav_html(nav, page, root, repo))
  doc.meta.links = raw(table.concat(links))
  local toc_s = toc_html(toc)
  doc.meta.toc = toc_s and raw(toc_s) or nil
  local pager = pager_html(nav, page, root)
  doc.meta.pager = pager and raw(pager) or nil
  doc.meta.section = section
  doc.meta.source = repo .. "/blob/main/" .. src
  doc.meta.srcname = src
  doc.meta.home = page == "index.html" and "true" or nil
  return doc
end
