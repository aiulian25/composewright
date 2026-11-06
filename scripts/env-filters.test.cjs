// Run with node --test scripts/env-filters.test.cjs. These DOM doubles test
// state transitions without a browser/server; native focus/layout still need
// browser verification. Go's TestEnvReviewFallback checks the real template.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const source = fs.readFileSync(path.join(__dirname, '../web/static/app.js'), 'utf8');
const start = source.indexOf('  function setUpEnvFilters()');
const end = source.indexOf('  // The sidebar filter', start);
assert(start >= 0 && end > start);

class Element {
  constructor(attrs = {}, parent = null) { this.attrs = attrs; this.parent = parent; this.handlers = {}; this.hidden = false; this.disabled = false; }
  get parentElement() { return this.parent; }
  getAttribute(key) { return this.attrs[key] ?? null; }
  hasAttribute(key) { return key in this.attrs; }
  addEventListener(type, callback, capture) { this.handlers[type] = callback; if (type === 'invalid') this.invalidCapture = capture; }
  emit(type, target = this, extra = {}) { return this.handlers[type]?.({target, ...extra}); }
  closest(selector) {
    if ((selector === '[data-env-row]' && this.hasAttribute('data-env-row')) ||
        (selector === 'details' && this.isDetails) || (selector === 'form' && this.isForm)) return this;
    return this.parent?.closest(selector) ?? null;
  }
  contains(target) { return !!target && (this === target || this.contains(target.parent)); }
}
function fixture(withReference = false) {
  const form = new Element(); form.isForm = true;
  const section = new Element({}, form), tools = new Element({}, section), search = new Element({}, tools), status = new Element({}, tools), clear = new Element({}, tools);
  search.value = ''; tools.hidden = true;
  const optional = new Element({}, section); optional.isDetails = true; optional.open = false;
  const keys = ['all', 'needs', 'optional', 'secret', 'changed'];
  const filters = keys.map(value => Object.assign(new Element({}, tools), {value, checked: value === 'all'}));
  const counts = keys.map(key => new Element({'data-env-count':key},tools));
  function row(name, value, flags = {}) {
    const node = new Element({'data-env-row':'', 'data-env-search':name + ' ' + (flags.description || ''),
      'data-env-secret':String(!!flags.secret), 'data-env-required':String(!!flags.required),
      'data-env-stored':String(!!flags.stored), 'data-env-kind':flags.supplied ? 'supplied' : 'plain',
      'data-env-question':String(!!flags.question), ...(flags.optional ? {'data-env-optional-row':''} : {})}, flags.optional ? optional : section);
    node.input = value === null ? null : Object.assign(new Element({}, node), {value});
    node.querySelector = () => node.input;
    return node;
  }
  const rows = [row('API_URL','',{required:true,description:'Endpoint address'}), row('MODE','auto',{question:true}),
    row('PASSWORD','',{secret:true,supplied:true,stored:true}), row('TOKEN','',{secret:true,supplied:true}),
    row('GENERATED',null,{secret:true,stored:true}), row('GLOBAL','en_GB'),
    row('TZ',null,{optional:true,description:'Timezone'}), row('METRICS',null,{optional:true,description:'Metrics endpoint'}), row('MAIL_TOKEN',null,{optional:true,secret:true})];
  const groups = [rows.slice(0,2),rows.slice(2,6),rows.slice(6)].map(members => Object.assign(new Element({},section), {querySelectorAll:() => members}));
  tools.querySelectorAll = () => counts;
  const elements = {'[data-env-tools]':tools,'[data-env-search-input]':search,'[data-env-optional]':optional,'[data-env-status]':status,'[data-env-clear]':clear};
  section.querySelector = selector => elements[selector];
  section.querySelectorAll = selector => selector === 'input[name="env-filter"]' ? filters : selector === '[data-env-group]' ? groups : selector === '[data-env-row]' ? rows : [];
  const links=[];
  const targets={};
  if (withReference) {
    const link=new Element({href:'#reference-target'});
    links.push(link);
    targets['reference-target']=rows[0];
    rows[0].input.focus=() => {document.activeElement=rows[0].input;};
    rows[0].scrollIntoView=() => {rows[0].scrolled=true;};
  }
  const document = {querySelector:() => section, querySelectorAll:() => links, getElementById:id => targets[id] || null, activeElement:null};
  let observer;
  const window = {setTimeout:callback => callback(), location:{hash:''}, handlers:{}, addEventListener(type, callback) { this.handlers[type]=callback; }};
  const context = {document, window, MutationObserver:class { constructor(callback) {observer=callback;} observe() {} }};
  vm.runInNewContext(source.slice(start,end) + '\nsetUpEnvFilters();',context);
  function select(key) { document.activeElement=null; filters.forEach(filter => {filter.checked = filter.value === key;}); filters.find(filter => filter.checked).emit('change'); }
  function find(text) { document.activeElement=search; search.value=text; search.emit('input'); }
  function count(key) { return Number(counts[keys.indexOf(key)].textContent.slice(1,-1)); }
  return {form,section,tools,rows,groups,filters,search,status,clear,optional,document,window,select,find,count,links,targets,observer:target => observer([{target}])};
}

test('counts, name/description search, optional disclosure and empty results', () => {
  const f = fixture(); assert.equal(f.tools.hidden,false); assert.equal(f.count('all'),9); assert.equal(f.count('needs'),3); assert.equal(f.count('secret'),4);
  f.select('optional'); assert.equal(f.optional.open,true); assert.equal(f.rows.filter(row => !row.hidden).length,3);
  f.find('ENDPOINT'); assert.equal(f.count('all'),2); assert.equal(f.count('optional'),1);
  f.find('nothing matches'); assert.equal(f.count('all'),0); assert(f.groups.every(group => group.hidden)); assert.match(f.status.textContent,/Clear filters/);
  f.clear.emit('click'); assert.equal(f.count('all'),9); assert(f.rows.every(row => !row.hidden));
});
test('filtering keeps edits, counts changes and never searches values', () => {
  const f=fixture(); f.rows[2].input.value='dummy-private-needle'; f.section.emit('input',f.rows[2].input);
  assert.equal(f.count('changed'),1); f.find('dummy-private-needle'); assert.equal(f.count('all'),0);
  f.clear.emit('click'); f.select('changed'); assert.equal(f.rows[2].hidden,false); assert.equal(f.rows[2].input.value,'dummy-private-needle'); assert.equal(f.rows[2].input.disabled,false);
  f.rows[2].input.value=''; f.section.emit('input',f.rows[2].input); assert.equal(f.count('changed'),0); assert.equal(f.count('needs'),3);
});
test('needs-input editing keeps focus visible until the user leaves the row', () => {
  const f=fixture(); f.select('needs'); f.document.activeElement=f.rows[0].input;
  f.rows[0].input.value='https://example.invalid'; f.section.emit('input',f.rows[0].input);
  assert.equal(f.count('needs'),2); assert.equal(f.rows[0].hidden,false); assert.match(f.status.textContent,/editing stays visible/);
  f.document.activeElement=f.search; f.section.emit('focusout',f.rows[0].input); assert.equal(f.rows[0].hidden,true);
});
test('native and inline errors clear filters and open the affected disclosure', () => {
  const f=fixture(); f.find('no match'); assert.equal(f.form.invalidCapture,true);
  f.form.emit('invalid',f.rows[0].input); assert.equal(f.rows[0].hidden,false); assert.equal(f.search.value,'');
  f.select('needs'); f.rows[8].attrs['aria-invalid']='true'; f.observer(f.rows[8]); assert.equal(f.rows[8].hidden,false); assert.equal(f.optional.open,true);
});
test('Enter searches without submitting the review form', () => {
  const f=fixture(); let prevented=false;
  f.search.emit('keydown', f.search, {key:'Enter',preventDefault:() => {prevented=true;}}); assert(prevented);
});

test('reference links reveal a filtered row and focus it without changing edits', () => {
  const f=fixture(true); f.rows[0].input.value='typed endpoint'; f.find('no match');
  assert.equal(f.rows[0].hidden,true);
  f.links[0].emit('click');
  assert.equal(f.rows[0].hidden,false); assert.equal(f.document.activeElement,f.rows[0].input);
  assert.equal(f.rows[0].input.value,'typed endpoint'); assert.equal(f.rows[0].scrolled,true);
});

test('reference navigation opens service details and handles history fragments', () => {
  const f=fixture(true);
  const service=new Element({},f.form); service.tagName='DETAILS'; service.open=false;
  const summary=new Element({},service); summary.focus=() => {f.document.activeElement=summary;};
  service.querySelector=selector => selector==='summary' ? summary : null;
  service.scrollIntoView=() => {service.scrolled=true;};
  f.targets['reference-service']=service;
  f.window.location.hash='#reference-service'; f.window.handlers.hashchange();
  assert.equal(service.open,true); assert.equal(f.document.activeElement,summary); assert.equal(service.scrolled,true);
});
test('reference links open optional disclosures and preserve the saved setting', () => {
  const f=fixture(true); f.find('no match');
  const target=f.rows[6], summary=new Element({},target);
  summary.focus=() => {f.document.activeElement=summary;};
  target.parent.tagName='DETAILS'; target.querySelector=selector => selector==='summary' ? summary : null;
  target.scrollIntoView=() => {};
  f.targets['reference-target']=target;
  f.links[0].emit('click');
  assert.equal(target.hidden,false); assert.equal(f.optional.open,true); assert.equal(f.document.activeElement,summary);
});
