package builder

import (
	"fmt"
	"path/filepath"
)

// StorefrontIslandAssetName is the public/ filename for the per-site
// storefront cart JS. Same naming pattern as VisitorHydrationAssetName
// and EngagementBeaconAssetName: an underscore prefix flags it as a
// build-managed asset (not authored content) and keeps it from
// colliding with author-uploaded files.
const StorefrontIslandAssetName = "_atomic-storefront.js"

// StorefrontIslandScript returns the vanilla JS module that powers the
// 4 Sprint 2C storefront blocks (product_grid, product_detail,
// cart_drawer, checkout_form). One single per-site asset wired into
// every page via <script src>. CSP-safe because it is same-origin,
// not inline.
//
// The script attaches global listeners on document.body for:
//
//	[data-cart-add]       -> push a snapshot item into the cart, open drawer
//	[data-cart-open]      -> open the drawer
//	[data-cart-close]     -> close the drawer
//	[data-cart-qty-inc]   -> increment qty for a variant
//	[data-cart-qty-dec]   -> decrement qty for a variant (remove at 0)
//	[data-cart-remove]    -> remove a variant entirely
//	[data-variant-pick]   -> swap the data-cart-item snapshot on the
//	                        nearby Add-to-cart button (variant picker)
//	submit [data-checkout-form]
//	                      -> POST cart + form fields to
//	                        /api/sites/{siteID}/checkout, then redirect
//	                        to the returned checkout_url.
//
// Cart state lives in localStorage under "atomic-cart-{siteID}" so it
// survives reloads + navigation. Each cart item is a full snapshot of
// the variant at Add-to-cart time (variant_id, product_id, name,
// variant_name, image_url, price_cents, currency, qty); the server
// re-validates prices + inventory at /checkout so a stale snapshot
// is rejected with a clear error rather than silently overcharging.
//
// Idle cost when a page has none of the data-cart-* attributes: zero
// (event delegation hits no matches). Module body is ~6 KB
// uncompressed, gzip ~2 KB, cached per browser session.
func StorefrontIslandScript(siteID string) string {
	return fmt.Sprintf(`(function(){
  var SITE_ID=%q;var KEY="atomic-cart-"+SITE_ID;
  function read(){try{var raw=localStorage.getItem(KEY);if(!raw)return [];var a=JSON.parse(raw);return Array.isArray(a)?a:[];}catch(_){return [];}}
  function write(items){try{localStorage.setItem(KEY,JSON.stringify(items));}catch(_){}}
  function dispatch(){document.dispatchEvent(new CustomEvent("atomic-cart-change"));}
  function add(snap){
    if(!snap||!snap.variant_id||!snap.price_cents)return;
    var qty=snap.qty&&snap.qty>0?Math.floor(snap.qty):1;
    var items=read();var found=false;
    for(var i=0;i<items.length;i++){if(items[i].variant_id===snap.variant_id){items[i].qty=(items[i].qty||1)+qty;found=true;break;}}
    if(!found){snap.qty=qty;items.push(snap);}
    write(items);dispatch();
  }
  function setQty(variantId,qty){
    var items=read();var out=[];
    for(var i=0;i<items.length;i++){
      if(items[i].variant_id===variantId){if(qty>0){items[i].qty=qty;out.push(items[i]);}}
      else{out.push(items[i]);}
    }
    write(out);dispatch();
  }
  function remove(variantId){
    var items=read();var out=[];
    for(var i=0;i<items.length;i++){if(items[i].variant_id!==variantId)out.push(items[i]);}
    write(out);dispatch();
  }
  function clear(){write([]);dispatch();}
  function subtotal(){
    var items=read();var cents=0;var currency="";
    for(var i=0;i<items.length;i++){cents+=items[i].price_cents*items[i].qty;if(!currency)currency=items[i].currency||"";}
    return {cents:cents,currency:currency};
  }
  function fmtPrice(cents,currency,locale){
    if(typeof cents!=="number"||!currency)return "";
    try{return new Intl.NumberFormat(locale||"en-US",{style:"currency",currency:currency}).format(cents/100);}
    catch(_){return (cents/100).toFixed(2)+" "+currency;}
  }
  function esc(s){return String(s==null?"":s).replace(/[&<>"]/g,function(c){return c==="&"?"&amp;":c==="<"?"&lt;":c===">"?"&gt;":"&quot;";});}
  function renderDrawer(drawer){
    var items=read();var locale=drawer.getAttribute("data-currency-locale")||"";
    var emptyEl=drawer.querySelector("[data-cart-empty]");
    var listEl=drawer.querySelector("[data-cart-items]");
    var totalEl=drawer.querySelector("[data-cart-total]");
    var checkoutEl=drawer.querySelector("[data-cart-checkout]");
    if(items.length===0){
      if(emptyEl)emptyEl.hidden=false;
      if(listEl){listEl.innerHTML="";listEl.hidden=true;}
      if(totalEl)totalEl.textContent="";
      if(checkoutEl)checkoutEl.hidden=true;
      return;
    }
    if(emptyEl)emptyEl.hidden=true;
    if(checkoutEl)checkoutEl.hidden=false;
    if(listEl){
      listEl.hidden=false;
      var html="";
      for(var i=0;i<items.length;i++){
        var it=items[i];var line=it.price_cents*it.qty;
        html+='<li class="cart-line" data-cart-line="'+esc(it.variant_id)+'">';
        if(it.image_url)html+='<img class="cart-line-img" src="'+esc(it.image_url)+'" alt="" />';
        html+='<div class="cart-line-body"><p class="cart-line-name">'+esc(it.name)+'</p>';
        if(it.variant_name)html+='<p class="cart-line-variant">'+esc(it.variant_name)+'</p>';
        html+='<p class="cart-line-price">'+esc(fmtPrice(line,it.currency,locale))+'</p>';
        html+='<div class="cart-line-qty"><button type="button" data-cart-qty-dec="'+esc(it.variant_id)+'" aria-label="Decrease quantity">-</button>';
        html+='<span>'+it.qty+'</span>';
        html+='<button type="button" data-cart-qty-inc="'+esc(it.variant_id)+'" aria-label="Increase quantity">+</button>';
        html+='<button type="button" class="cart-line-remove" data-cart-remove="'+esc(it.variant_id)+'" aria-label="Remove">&times;</button></div></div></li>';
      }
      listEl.innerHTML=html;
    }
    var t=subtotal();
    if(totalEl)totalEl.textContent=fmtPrice(t.cents,t.currency,locale);
  }
  function openDrawer(){
    var drawer=document.querySelector("[data-cart-drawer]");
    if(!drawer)return;
    renderDrawer(drawer);
    if(typeof drawer.showModal==="function"&&!drawer.open)drawer.showModal();
    else drawer.setAttribute("open","");
  }
  function closeDrawer(drawer){
    if(!drawer)return;
    if(typeof drawer.close==="function"&&drawer.open)drawer.close();
    else drawer.removeAttribute("open");
  }
  function snapshotFromBtn(btn){
    var raw=btn.getAttribute("data-cart-item");if(!raw)return null;
    try{return JSON.parse(raw);}catch(_){return null;}
  }
  document.addEventListener("click",function(e){
    var t=e.target;if(!t||!(t instanceof Element))return;
    var addBtn=t.closest("[data-cart-add]");
    if(addBtn){e.preventDefault();var snap=snapshotFromBtn(addBtn);if(snap){add(snap);openDrawer();}return;}
    var openBtn=t.closest("[data-cart-open]");
    if(openBtn){e.preventDefault();openDrawer();return;}
    var closeBtn=t.closest("[data-cart-close]");
    if(closeBtn){e.preventDefault();closeDrawer(closeBtn.closest("[data-cart-drawer]"));return;}
    var inc=t.closest("[data-cart-qty-inc]");
    if(inc){var id=inc.getAttribute("data-cart-qty-inc");var items=read();for(var i=0;i<items.length;i++)if(items[i].variant_id===id){setQty(id,items[i].qty+1);return;}return;}
    var dec=t.closest("[data-cart-qty-dec]");
    if(dec){var id2=dec.getAttribute("data-cart-qty-dec");var items2=read();for(var j=0;j<items2.length;j++)if(items2[j].variant_id===id2){setQty(id2,items2[j].qty-1);return;}return;}
    var rm=t.closest("[data-cart-remove]");
    if(rm){remove(rm.getAttribute("data-cart-remove"));return;}
    var pick=t.closest("[data-variant-pick]");
    if(pick){
      var raw=pick.getAttribute("data-cart-item");if(!raw)return;
      var scope=pick.closest("[data-product-detail]")||document;
      var target=scope.querySelector("[data-cart-add]");
      if(target)target.setAttribute("data-cart-item",raw);
      var priceEl=scope.querySelector("[data-product-price]");
      try{var s=JSON.parse(raw);if(priceEl&&s&&s.price_cents)priceEl.textContent=fmtPrice(s.price_cents,s.currency,priceEl.getAttribute("data-currency-locale")||"");}catch(_){}
      return;
    }
  });
  document.addEventListener("change",function(e){
    var t=e.target;if(!t||!(t instanceof Element))return;
    var radio=t.closest("input[data-variant-pick]");
    if(radio&&radio.checked){
      var raw=radio.getAttribute("data-cart-item");if(!raw)return;
      var scope=radio.closest("[data-product-detail]")||document;
      var target=scope.querySelector("[data-cart-add]");
      if(target)target.setAttribute("data-cart-item",raw);
      var priceEl=scope.querySelector("[data-product-price]");
      try{var s=JSON.parse(raw);if(priceEl&&s&&s.price_cents)priceEl.textContent=fmtPrice(s.price_cents,s.currency,priceEl.getAttribute("data-currency-locale")||"");}catch(_){}
    }
  });
  document.addEventListener("atomic-cart-change",function(){
    var drawer=document.querySelector("[data-cart-drawer]");if(drawer&&drawer.open)renderDrawer(drawer);
    var badges=document.querySelectorAll("[data-cart-count]");
    var items=read();var count=0;for(var i=0;i<items.length;i++)count+=items[i].qty;
    for(var j=0;j<badges.length;j++){badges[j].textContent=count>0?String(count):"";badges[j].hidden=count===0;}
  });
  document.addEventListener("submit",function(e){
    var form=e.target;if(!form||!form.matches||!form.matches("[data-checkout-form]"))return;
    e.preventDefault();
    var siteId=form.getAttribute("data-site-id")||SITE_ID;
    var errBox=form.querySelector("[data-checkout-error]");
    if(errBox){errBox.textContent="";errBox.hidden=true;}
    var items=read();
    if(items.length===0){if(errBox){errBox.textContent="Your cart is empty.";errBox.hidden=false;}return;}
    var fd=new FormData(form);
    var body={
      items:items.map(function(it){return {variant_id:it.variant_id,quantity:it.qty};}),
      customer:{name:String(fd.get("name")||"").trim(),email:String(fd.get("email")||"").trim(),phone:String(fd.get("phone")||"").trim()},
      discount_code:String(fd.get("discount_code")||"").trim(),
      return_url:form.getAttribute("data-return-url")||"",
      locale:document.documentElement.lang||""
    };
    if(form.getAttribute("data-require-shipping")==="1"){
      body.shipping_address={
        street:String(fd.get("ship_street")||"").trim(),
        city:String(fd.get("ship_city")||"").trim(),
        postal_code:String(fd.get("ship_postal_code")||"").trim(),
        country:String(fd.get("ship_country")||"").trim().toUpperCase()
      };
    }
    var submitBtn=form.querySelector("[type=submit]");
    var originalLabel="";
    if(submitBtn){originalLabel=submitBtn.textContent||"";submitBtn.disabled=true;}
    fetch("/api/sites/"+encodeURIComponent(siteId)+"/checkout",{
      method:"POST",headers:{"Content-Type":"application/json","Accept":"application/json"},
      body:JSON.stringify(body),credentials:"omit"
    }).then(function(r){
      return r.json().then(function(d){return {ok:r.ok,status:r.status,data:d};},function(){return {ok:r.ok,status:r.status,data:{}};});
    }).then(function(res){
      if(submitBtn){submitBtn.disabled=false;submitBtn.textContent=originalLabel;}
      if(res.ok&&res.data&&res.data.checkout_url){clear();window.location.href=res.data.checkout_url;return;}
      if(res.ok&&res.data&&res.data.order){clear();var u=body.return_url||"/";var sep=u.indexOf("?")>=0?"&":"?";window.location.href=u+sep+"order="+encodeURIComponent(res.data.order.order_number||"");return;}
      var msg=(res.data&&(res.data.error||res.data.message))||"Checkout failed. Please try again.";
      if(errBox){errBox.textContent=msg;errBox.hidden=false;}
    }).catch(function(){
      if(submitBtn){submitBtn.disabled=false;submitBtn.textContent=originalLabel;}
      if(errBox){errBox.textContent="Network error. Please try again.";errBox.hidden=false;}
    });
  });
  function initialRender(){
    var drawer=document.querySelector("[data-cart-drawer]");if(drawer)renderDrawer(drawer);
    document.dispatchEvent(new CustomEvent("atomic-cart-change"));
  }
  if(document.readyState!=="loading"){initialRender();}else{document.addEventListener("DOMContentLoaded",initialRender);}
  /* Astro ClientRouter (showcase fidelity) swaps the body without firing
     DOMContentLoaded again; re-render badge + drawer on soft navs. The
     delegated listeners above survive swaps (bound on document). No-op
     on sites without the router. */
  document.addEventListener("astro:page-load",initialRender);
  window.AtomicCart={items:read,add:add,setQty:setQty,remove:remove,clear:clear,subtotal:subtotal};
})();
`, siteID)
}

// WriteStorefrontIslandAsset writes the per-site storefront cart JS to
// the workspace's public/ dir so pages can reference it via same-origin
// <script src>. Mirrors WriteVisitorHydrationAsset.
func WriteStorefrontIslandAsset(workspaceDir, siteID string) error {
	publicDir := filepath.Join(workspaceDir, "public")
	if err := EnsureDir(publicDir); err != nil {
		return fmt.Errorf("ensure public dir: %w", err)
	}
	target := filepath.Join(publicDir, StorefrontIslandAssetName)
	return WriteFile(target, StorefrontIslandScript(siteID))
}

// RenderStorefrontIslandTag returns the <script> tag that pulls in the
// per-site storefront cart asset. Same is:inline + defer pattern as
// RenderVisitorHydration so the CSP-driven externalisation works
// identically.
func RenderStorefrontIslandTag() string {
	return fmt.Sprintf("  <script is:inline defer src=\"/%s\"></script>\n", StorefrontIslandAssetName)
}
