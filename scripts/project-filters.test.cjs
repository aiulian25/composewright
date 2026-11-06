// Run with node scripts/project-filters.test.cjs. State transitions use DOM
// doubles; native keyboard/layout behavior still requires browser checking.
const {test}=require('node:test');
const assert=require('node:assert/strict');
const fs=require('node:fs');
const path=require('node:path');
const vm=require('node:vm');
const source=fs.readFileSync(path.join(__dirname,'../web/static/app.js'),'utf8');
const start=source.indexOf('  function setUpProjectFilter()');
const end=source.indexOf('  // Menus close',start);
assert(start>=0&&end>start);
class Element {
 constructor(attrs={}) {this.attrs=attrs;this.handlers={};this.value='';this.hidden=false;}
 getAttribute(key) {return this.attrs[key]??null;}
 addEventListener(type,callback) {this.handlers[type]=callback;}
 emit(type,event={}) {this.handlers[type]?.(event);}
}
function fixture() {
 const name=new Element(),status=new Element(),label=new Element(),result=new Element(),form=new Element(),clear=new Element();
 const items=[
  new Element({'data-project-name':'beta','data-project-status':'failed','data-project-labels':'media','data-project-current':'true'}),
  new Element({'data-project-name':'alpha','data-project-status':'passed','data-project-labels':'home,media','data-project-current':'false'}),
  new Element({'data-project-name':'gamma','data-project-status':'passed','data-project-labels':'home','data-project-current':'false'})];
 items.forEach(item=>{item.textContent=item.getAttribute('data-project-name');});
 items[0].href='/projects/beta/validate';items[0].ariaCurrent='page';
 const groups=[items.slice(0,2),items.slice(2)].map(rows=>Object.assign(new Element(),{querySelectorAll:()=>rows}));
 const controls={'[data-project-filter]':name,'select[data-project-status]':status,'select[data-project-label]':label,'[data-project-filter-status]':result,'[data-project-filter-form]':form,'[data-project-clear]':clear};
 const collection={querySelector:key=>controls[key],querySelectorAll:key=>key==='[data-project-item]'?items:groups};
 vm.runInNewContext(source.slice(start,end)+'\nsetUpProjectFilter();',{document:{querySelector:key=>key==='[data-project-collection]'?collection:key==='[data-project-filter]'?name:key==='[data-project-list]'?{querySelectorAll:()=>items}:null}});
 return {name,status,label,result,form,clear,items,groups};
}
test('combines name, status and labels while retaining the current navigation',()=>{
 const f=fixture();f.name.value='AL';f.status.value='passed';f.label.value='media';f.name.emit('input');
 assert.equal(f.items[1].hidden,false);assert.equal(f.items[2].hidden,true);assert.equal(f.items[0].hidden,false);
 assert.match(f.result.textContent,/1 of 3/);assert.match(f.result.textContent,/current project stays visible/);
 assert.equal(f.items[0].href,'/projects/beta/validate');assert.equal(f.items[0].ariaCurrent,'page');assert.equal(f.groups[1].hidden,true);
});
test('empty results preserve the current project and clear restores both groups',()=>{
 const f=fixture();f.label.value='missing';f.label.emit('change');assert.match(f.result.textContent,/0 of 3/);
 assert.equal(f.items[0].hidden,false);assert.equal(f.items[1].hidden,true);assert.equal(f.items[2].hidden,true);
 let prevented=false;f.clear.emit('click',{preventDefault:()=>{prevented=true;}});
 assert(prevented);assert.equal(f.label.value,'');assert(f.items.every(item=>!item.hidden));assert(f.groups.every(group=>!group.hidden));
});
test('filter submission never changes the page or link order',()=>{
 const f=fixture();const order=[...f.items];let prevented=false;
 f.status.value='failed';f.form.emit('submit',{preventDefault:()=>{prevented=true;}});
 assert(prevented);assert.deepEqual(f.items,order);assert.equal(f.items[0].hidden,false);assert.equal(f.items[1].hidden,true);
});
