"""Bounded behavioral mutations for exact planning, stock search and WZ safety."""
import argparse
import hashlib
import json
from pathlib import Path
import subprocess
import tempfile

from verify_ui_mutations import classify


def catalog():
    return [
        ('recipe_quantity_ignored','units(parseQuantity(component.quantity)) * amount','units(parseQuantity(component.quantity)) * 1000000n'),
        ('recipe_float_rounding','units(parseQuantity(component.quantity)) * amount','BigInt(Number(units(parseQuantity(component.quantity)))) * amount'),
        ('recipe_duplicate_material_lost',"(requirements.get(component.item_id) || 0n) + product / 1000000n",'product / 1000000n'),
        ('recipe_micro_fraction_truncated',"if (product % 1000000n) error('Ilość materiału przekracza dokładność magazynu');",''),
        ('availability_cross_owner',' && row.owner_id === scope.owner_id',''),
        ('availability_cross_warehouse',' && row.warehouse_id === scope.warehouse_id',''),
        ('availability_uses_physical_not_free','sum + units(row.available_qty)','sum + units(row.on_hand)'),
        ('shortage_surplus_negative','units(line.quantity) > available ? units(line.quantity) - available : 0n','units(line.quantity) - available'),
        ('stock_location_search_removed','item?.name,position?.code,position?.name','item?.name'),
        ('stock_diacritics_not_folded',".normalize('NFD').replace(/[\\u0300-\\u036f]/g,'').replace(/ł/g,'l')",''),
        ('reservation_ignores_reserved_quantity','remaining(remaining(line.requested_qty,line.fulfilled_qty),line.reserved_qty)','remaining(line.requested_qty,line.fulfilled_qty)'),
        ('reservation_uses_physical_stock','units(input.sourceBalance.available_qty)','units(input.sourceBalance.on_hand)'),
        ('shipment_source_owner_unchecked',"['organization_id','warehouse_id','owner_id'].some(key => sourceBalance[key] !== document[key])","['organization_id','warehouse_id'].some(key => sourceBalance[key] !== document[key])"),
        ('issue_reservation_other_line','reservation.document_line_id !== line.id','false'),
        ('issue_reservation_other_balance','reservation.balance_id !== sourceBalance.id','false'),
        ('issue_reservation_active_quantity_unchecked','units(amount) > units(reservation.active_qty) || ','') ,
        ('issue_wrong_reservation_id','reservation_id:reservation.id,quantity:amount','reservation_id:sourceBalance.id,quantity:amount'),
        ('issue_physical_stock_unchecked',"if (units(amount) > units(sourceBalance.on_hand)) error('Pod adresem nie ma wystarczającej ilości towaru');",''),
    ]


def main():
    parser=argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root',type=Path,default=Path(__file__).resolve().parents[1])
    parser.add_argument('--node',default='node')
    parser.add_argument('--output',type=Path,default=Path('work/industry-ui-mutations.json'))
    args=parser.parse_args();root=args.root.resolve()
    source=root/'web/src/domain.js';test=root/'web/tests/industry-domain.test.mjs'
    content=source.read_text(encoding='utf8')
    hashes={p.relative_to(root).as_posix():hashlib.sha256(p.read_bytes()).hexdigest() for p in [source,test,Path(__file__).resolve()]}
    report={'passed':False,'scope':'Planning arithmetic, availability, stock search and shipment intents; not a whole-UI mutation score.','source_sha256':hashes,'mutations':[],'killed':0,'survived':0,'invalid':0,'skipped':0}
    parent=root/'web/work';parent.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix='industry-mutations-',dir=parent) as temporary:
        isolated=Path(temporary).resolve();isolated.relative_to(parent.resolve())
        (isolated/'src').mkdir();(isolated/'tests').mkdir()
        (isolated/'package.json').write_text('{"type":"module"}\n',encoding='utf8')
        target=isolated/'src/domain.js';target.write_text(content,encoding='utf8')
        (isolated/'tests/industry-domain.test.mjs').write_bytes(test.read_bytes())
        def run():
            result=subprocess.run([args.node,'--test','--test-reporter=tap','tests/industry-domain.test.mjs'],cwd=isolated,capture_output=True,text=True,encoding='utf8',errors='replace',timeout=40)
            return result.returncode,result.stdout+result.stderr
        code,output=run();count=10
        report['baseline']={'status':classify(code,output,count),'exit_code':code,'tests':count,'output':output}
        if report['baseline']['status']!='survived':
            report['invalid']+=1
        else:
            for name,before,after in catalog():
                if content.count(before)!=1:
                    record={'name':name,'status':'invalid','reason':'Reviewed anchor changed'}
                else:
                    changed=content.replace(before,after,1);target.write_text(changed,encoding='utf8')
                    try:
                        code,output=run()
                        record={'name':name,'status':classify(code,output,count),'exit_code':code,'mutant_sha256':hashlib.sha256(changed.encode()).hexdigest(),'output':output}
                    except subprocess.TimeoutExpired:
                        record={'name':name,'status':'invalid','reason':'timeout'}
                    finally:target.write_text(content,encoding='utf8')
                report['mutations'].append(record);report[record['status']]+=1
                print(name+': '+record['status'],flush=True)
    report['passed']=report['killed']==len(catalog()) and report['survived']==report['invalid']==0
    assert all(hashlib.sha256((root/name).read_bytes()).hexdigest()==expected for name,expected in hashes.items()),'Source changed during mutation campaign'
    report['source_unchanged']=True
    args.output.parent.mkdir(parents=True,exist_ok=True)
    args.output.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n',encoding='utf8')
    print(json.dumps({key:report[key] for key in ['passed','killed','survived','invalid','skipped']}))
    return 0 if report['passed'] else 1


if __name__=='__main__':raise SystemExit(main())
