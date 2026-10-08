import fs from 'node:fs/promises';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const dir=path.resolve(process.argv[2]||path.join(path.dirname(fileURLToPath(import.meta.url)),'../../dist/verification/bills'));
const source=JSON.parse(await fs.readFile(path.join(dir,'expected-bills.json'),'utf8'));
const expected=process.env.BILL_ID?source.filter(b=>b.id===process.env.BILL_ID):source;
if(!expected.length)throw Error('No matching source bill: '+process.env.BILL_ID);
const base=process.env.ERPNEXT_URL||'http://127.0.0.1:8000';
const itemCodes={BILL_01:['Cloud Compute Instances','Managed Database Hosting'],BILL_02:['Developer Workstation','4K USB-C Monitor'],BILL_03:['A4 Printing Paper','Ergonomic Office Chair'],BILL_04:['Statutory Audit & GST Compliance'],BILL_05:['Internet Leased Line']};
const login=await fetch(base+'/api/method/login',{method:'POST',body:new URLSearchParams({usr:process.env.ERPNEXT_USER||'Administrator',pwd:process.env.ERPNEXT_PASSWORD||'admin'}),signal:AbortSignal.timeout(15000)});
if(!login.ok)throw Error('ERPNext login failed: '+login.status);
const cookie=login.headers.getSetCookie().map(c=>c.split(';')[0]).join('; ');
async function get(p){const r=await fetch(base+p,{headers:{Cookie:cookie},signal:AbortSignal.timeout(15000)});if(!r.ok)throw Error('ERPNext read failed: '+r.status+' '+p);return (await r.json()).data;}
const list=await get('/api/resource/Purchase%20Invoice?limit_page_length=1000&fields='+encodeURIComponent(JSON.stringify(['name','supplier','bill_no','docstatus'])));
const money=(a,b)=>Math.abs(Number(a||0)-Number(b||0))<0.01;
const date=s=>{const [d,m,y]=s.split('-');return y+'-'+String(['Jan','Feb','Mar','Apr','May','Jun','Jul','Aug','Sep','Oct','Nov','Dec'].indexOf(m)+1).padStart(2,'0')+'-'+d.padStart(2,'0');};
const plain=s=>String(s||'').replace(/<[^>]+>/g,' ').replace(/&(amp|lt|gt|quot|apos|nbsp);/g,(_,e)=>({amp:'&',lt:'<',gt:'>',quot:'"',apos:"'",nbsp:' '}[e])).replace(/&#(\d+);/g,(_,n)=>String.fromCodePoint(Number(n))).replace(/\s+/g,' ').trim();
const results=[];
for(const bill of expected){
 const matches=list.filter(i=>i.bill_no===bill.invoice_no&&i.docstatus!==2);
 const failures=[];const check=(ok,label)=>{if(!ok)failures.push(label);};
 check(matches.length===1,'Expected exactly one active invoice, found '+matches.length);
 let invoice;
 if(matches.length===1){
  invoice=await get('/api/resource/Purchase%20Invoice/'+encodeURIComponent(matches[0].name));
  check(invoice.supplier===bill.vendor.name,'Supplier');
  check(invoice.bill_date===date(bill.invoice_date),'Supplier invoice date');
  check(invoice.due_date===date(bill.due_date),'Due date');
  check(invoice.docstatus===0,'Expected saved draft');
  check(invoice.company==='Example Inc','Dataset ERP company');
  check(!invoice.update_stock,'Stock updates must remain off');
  check(invoice.supplier_gstin===bill.vendor.gstin,'Supplier GSTIN');
  check(invoice.company_gstin===bill.customer.gstin,'Company GSTIN');
  check(money(invoice.net_total,bill.subtotal),'Net total');
  check(money(invoice.total_taxes_and_charges,bill.total_tax),'Total tax');
  check(money(invoice.grand_total,bill.grand_total),'Grand total');
  check(money(invoice.rounded_total||invoice.grand_total,bill.grand_total),'Payable total');
  check(invoice.items.length===bill.items.length,'Line count');
  bill.items.forEach((line,i)=>{const actual=invoice.items[i];if(!actual)return;
   check(actual.item_code===itemCodes[bill.id]?.[i],'Line '+(i+1)+' item master');
   check(money(actual.qty,line.qty),'Line '+(i+1)+' quantity');
   check(actual.uom===line.unit,'Line '+(i+1)+' billing unit');
   for(const tax of ['cgst','sgst','igst']){
    const wanted=Number(bill[tax+'_amount'])?Number(line.gst_rate)/(tax==='igst'?1:2):0;
    const rows=(invoice.taxes||[]).filter(t=>t.gst_tax_type===tax);
    const itemRate=actual[tax+'_rate']??rows.reduce((sum,t)=>sum+Number((JSON.parse(t.item_wise_tax_detail||'{}')[actual.item_code]||[])[0]||0),0);
    check(money(itemRate,wanted),'Line '+(i+1)+' '+tax.toUpperCase()+' rate');
    check(money(actual[tax+'_amount'],Number(line.amount)*wanted/100),'Line '+(i+1)+' '+tax.toUpperCase()+' amount');
   }
   check(money(actual.rate,line.rate),'Line '+(i+1)+' rate');
   check(money(actual.amount,line.amount),'Line '+(i+1)+' amount');
   check(String(actual.gst_hsn_code||'').startsWith(line.hsn_sac),'Line '+(i+1)+' HSN/SAC');
   check(plain(actual.description)===plain(line.description),'Line '+(i+1)+' description');
  });
  for(const tax of ['cgst','sgst','igst']){const amount=(invoice.taxes||[]).filter(t=>t.gst_tax_type===tax||new RegExp('\\b'+tax+'\\b','i').test(t.account_head||'')).reduce((sum,t)=>sum+Number(t.tax_amount||0),0);check(money(amount,bill[tax+'_amount']),tax.toUpperCase()+' amount');}
 }
 results.push({bill:bill.id,invoice_no:bill.invoice_no,invoice:invoice?.name,expected_total:bill.grand_total,actual_total:invoice?.grand_total,pass:failures.length===0,failures});
}
const report={checked_at:new Date().toISOString(),passed:results.filter(r=>r.pass).length,total:results.length,results};
await fs.writeFile(path.join(dir,'verification.json'),JSON.stringify(report,null,2));
console.log(JSON.stringify(report,null,2));
if(report.passed!==report.total)process.exitCode=1;
