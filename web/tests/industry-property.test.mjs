import test from 'node:test';
import assert from 'node:assert/strict';
import fc from 'fast-check';
import {materialRequirements,materialAvailability} from '../src/domain.js';

test('property: recipe totals match an independent integer multiplication oracle', () => {
  fc.assert(fc.property(fc.bigInt({min:1n,max:10000000000000000n}),fc.integer({min:1,max:50}),(perUnit,amount)=>{
    assert.deepEqual(materialRequirements({components:[{item_id:'screw',quantity:String(perUnit)}]},String(amount)),[{item_id:'screw',quantity:String(perUnit*BigInt(amount))}]);
  }),{seed:20271029,numRuns:160});
});

test('property: available stock partitions preserve exact material shortages',()=>{
  fc.assert(fc.property(fc.array(fc.integer({min:0,max:10000}),{minLength:1,maxLength:40}),fc.integer({min:1,max:100000}),(quantities,demand)=>{
    const scope={organization_id:'organization',warehouse_id:'warehouse',owner_id:'owner'};
    const balances=quantities.map(quantity=>({...scope,item_id:'screw',available_qty:String(quantity)}));
    const total=quantities.reduce((sum,value)=>sum+value,0);
    assert.deepEqual(materialAvailability([{item_id:'screw',quantity:String(demand)}],balances,scope),[{item_id:'screw',quantity:String(demand),available_qty:String(total),shortage_qty:String(Math.max(0,demand-total))}]);
  }),{seed:20271030,numRuns:160});
});
